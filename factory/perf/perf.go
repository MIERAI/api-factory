// Package perf: 性能闸门(P4)。接近真实的数据量 + EXPLAIN ANALYZE。
//
// 和差分测试相反: 数据追求真实的基数(主键唯一、普通列上千种值、日期分布几年),不追求陷阱值;
// 执行计划用正常的设置(打开索引扫描)。每个声明式 API 跑两种请求: 只有必填参数 / 全部参数。
//
// 规则:
//   - 执行时间 > 阈值                         → ✗
//   - 排序溢出到磁盘(Sort Method: external)  → ✗
//   - 有必填的筛选条件,却对大表全表扫描        → ⚠(提示要加的索引)
//   - JSON版本 的表的索引用了 JSON 键,却不是查询里的式子(core.JSONKeyIndexSQL)→ ✗(keyindex.go)
package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

type Options struct {
	DB       string
	FillOnly bool    // 只灌数据(迁移排练 / 恢复演练的数据量),不跑 EXPLAIN。所有列都填(包括没有 API 读的列)
	Dirty    bool    // 掺"脏"数据: 可为空的列 5% 空值、空字符串 / 前后空格 / 超长文本 / 0 / 负数 / 极端日期(约束允许的范围内)
	Rows     int     // 每张物理表的行数
	MaxMs    float64 // 执行时间的阈值(这台机器 = 参考机器时)
	Scale    float64 // 阈值的倍率: 这台机器比参考机器慢几倍(0 = Calibrate;≥ 1)

	CalibrationMs float64 // Calibrate 量到的时间(输出)

	// 写入 API 的并发检查(concurrency.go): Workers 路(0 = 不做),每路 WriteRequests 个请求;N 路 ÷ 1 路的吞吐 ≥ MinScale
	Workers       int
	WriteRequests int
	MinScale      float64
	Pick          func(n, i int) int // 第 i 个请求用 n 个候选行里的哪一个(nil = 每个请求不同的行)
}

type Report struct {
	API      string
	Variant  string // 必填参数 / 全部参数
	Ms       float64
	Rows     int // 返回的行数
	Fail     []string
	Warn     []string
	Plan     string // 计划的摘要(节点类型)
	Boundary string // 已知边界(原因是工厂的边界,不是回归;写明原因)

	seqScanOnly bool   // the failure is a sequential scan (known boundary for 包含)
	slow        string // execution time over the threshold (a failure unless a known boundary explains it)
}

// Run: fill the database, then EXPLAIN every API. opt.Scale = 0: calibrated here (written back).
func Run(ctx context.Context, plan *ir.Plan, o *Options) ([]Report, error) {
	opt := *o
	cfg, err := pgx.ParseConfig(opt.DB)
	if err != nil {
		return nil, fmt.Errorf("perf db: %w", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec // 和服务器一样: 匿名预编译语句,每次按实际参数做计划
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("perf db: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		return nil, err
	}
	tables, err := Fill(ctx, conn, plan, opt)
	if err != nil {
		return nil, err
	}
	if opt.FillOnly {
		return nil, nil
	}
	if opt.Scale <= 0 {
		if opt.Scale, o.CalibrationMs, err = Calibrate(ctx, conn); err != nil {
			return nil, err
		}
		o.Scale = opt.Scale
	}
	var out []Report
	for i := range plan.APIs {
		api := &plan.APIs[i]
		switch {
		case api.Pipeline != nil:
			opts, err := optionalParams(api)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", api.ID, err)
			}
			variants := []string{"", "*"}
			if len(opts) > 1 { // 只有 1 个可选参数时,"全部参数"就是它
				variants = append(variants, opts...)
			}
			for _, v := range variants {
				r, err := explain(ctx, conn, plan, api, v, opt, tables)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", api.ID, err)
				}
				out = append(out, r)
			}
		case api.Query.Escape && api.Query.SQL != "":
			r, err := explainEscape(ctx, conn, api, opt, tables)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", api.ID, err)
			}
			out = append(out, r)
		case api.Command != nil: // after all the reads (the write checks change the data)
		default:
			out = append(out, unknownKind(api))
		}
	}
	ks, err := keyIndexes(ctx, conn, plan) // JSON 键的表达式索引 = 查询里的式子(keyindex.go)
	if err != nil {
		return nil, err
	}
	out = append(out, ks...)
	ws, err := checkWrites(ctx, conn, plan, tables, opt)
	if err != nil {
		return nil, err
	}
	return append(out, ws...), nil
}

// Fill: the perf database's data (every table of the schema, n rows each). Returns the tables (generator's view).
func Fill(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, opt Options) (map[string]*table, error) {
	tables, err := loadSchema(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("perf schema: %w", err)
	}
	for _, t := range tables {
		t.n, t.dirty = opt.Rows, opt.Dirty
	}
	if err := applyPlan(plan, tables); err != nil {
		return nil, err
	}
	order, err := fillOrder(tables)
	if err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return tables, nil
	}
	var names []string
	for _, t := range order {
		names = append(names, pgx.Identifier{t.name}.Sanitize())
	}
	if _, err := conn.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		return nil, err
	}
	for _, t := range order {
		if err := fill(ctx, conn, t); err != nil {
			return nil, fmt.Errorf("数据 %s: %w", t.name, err)
		}
	}
	return tables, nil
}

// quantile: 筛选的种类 → 取样的分位。
func quantile(op string) float64 {
	switch op {
	case "gt", "gte":
		return 0.4
	case "lt", "lte":
		return 0.6
	}
	return 0.5
}

// versionScan: 已知边界 —— 每组取一条(取最新版本)之后的筛选,只有引用分组键的能下推(core/pushdown.go);
// 非键的条件必须先给全部的键取最新版本再判断,成本 = 全部版本。请求里有非键条件、却没有键上的条件时就是这种情况。
// 解决要靠"最新版本"物化表(外: docs/memo/out-of-scope.md)或按参数组合生成多条 SQL。
func versionScan(api *ir.API, given []string) string {
	keys := map[string]bool{}
	for _, st := range api.Pipeline.Steps {
		if st.Kind == "pick_per_key" {
			for _, k := range st.Keys {
				keys[k] = true
			}
		}
	}
	if len(keys) == 0 {
		return ""
	}
	var nonKey []string
	for _, c := range given {
		if keys[c] {
			return ""
		}
		nonKey = append(nonKey, c)
	}
	if len(nonKey) == 0 {
		return ""
	}
	return fmt.Sprintf("已知边界: 取最新版本之后的非键筛选(%s)要扫全部版本", strings.Join(nonKey, ", "))
}

// joinVersionScan: 已知边界 —— 关联 / 嵌套 / 子表合计的对方是逻辑表(取最新版本),关联键却不是对方的键:
// 每一行都要给对方全部的键取最新版本再比较,成本 = 对方的全部版本 × 行数。解决同上(物化表)。
func joinVersionScan(api *ir.API) string {
	for _, st := range api.Pipeline.Steps {
		if len(st.Source) == 0 {
			continue
		}
		keys := map[string]bool{}
		for _, s := range st.Source {
			if s.Kind == "pick_per_key" {
				for _, k := range s.Keys {
					keys[k] = true
				}
			}
		}
		if len(keys) == 0 {
			continue
		}
		for _, on := range st.On {
			if !keys[on.Key] {
				return fmt.Sprintf("已知边界: %s 的对方 %s 是逻辑表,关联键 %s 不是它的键,每行都要扫对方的全部版本", st.Kind, st.Table, on.Key)
			}
		}
	}
	return ""
}

// required: 参数是不是必填(流水线里必填,或者请求的校验规则必填)。
func required(api *ir.API, d core.ParamDecl) bool {
	if fd := api.Request.Field(d.Name); fd != nil && fd.Rules != nil && fd.Rules.Required {
		return true
	}
	return d.Required
}

// optionalParams: 筛选用的可选参数(每个单独加上必填参数,跑一次: 只传一个可选条件往往是最坏的情况)。
func optionalParams(api *ir.API) ([]string, error) {
	q, err := core.ToSQL(api.Pipeline, core.Options{})
	if err != nil {
		return nil, err
	}
	filters := map[string]string{} // 参数 → 变体名(组属于: 每个字段的参数 → 组的输入名)
	for _, f := range api.Filters {
		if f.Op == "in_tuples" {
			for _, c := range f.Columns {
				filters[ir.MemberParam(f.Field, c)] = f.Field
			}
			continue
		}
		filters[f.Field] = f.Field
	}
	var out []string
	for _, d := range q.Params {
		if v, ok := filters[d.Name]; ok && !required(api, d) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, nil
}

func sqlArg(v any) any {
	switch x := v.(type) {
	case *big.Rat:
		return core.RatString(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case []any: // 匿名语句不知道参数的类型: 列表一律用文本,由 SQL 里的 ::类型[] 解析
		out := make([]string, len(x))
		for i, y := range x {
			out[i] = fmt.Sprint(sqlArg(y))
		}
		return out
	}
	return v
}

// refCalibrationMs: calibrationSQL on the reference machine (the factory's development Mac, Postgres 16 in Docker).
const refCalibrationMs = 150.0

const calibrationSQL = "SELECT sum(x * 2 + 1) FROM generate_series(1, 2000000) AS x"

// Calibrate: how many times slower this database is than the reference machine (≥ 1; the fastest of 5 runs). The time
// threshold is MaxMs × Scale, so the same gate holds on a laptop and on a shared CI runner; the plan rules do not scale.
func Calibrate(ctx context.Context, conn *pgx.Conn) (float64, float64, error) {
	best := -1.0
	for range 5 {
		var raw string
		if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+calibrationSQL).Scan(&raw); err != nil {
			return 0, 0, err
		}
		var doc []struct {
			ExecutionTime float64 `json:"Execution Time"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) == 0 {
			return 0, 0, fmt.Errorf("calibrate: %v", err)
		}
		if best < 0 || doc[0].ExecutionTime < best {
			best = doc[0].ExecutionTime
		}
	}
	return max(1, best/refCalibrationMs), best, nil
}

func enumValues(en *ir.Enum) []int64 {
	var out []int64
	for _, v := range en.Values {
		out = append(out, v.DB)
	}
	return out
}
