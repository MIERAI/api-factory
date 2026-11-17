// Package perf: 性能闸门(P4)。接近真实的数据量 + EXPLAIN ANALYZE。
//
// 和差分测试相反: 数据追求真实的基数(主键唯一、普通列上千种值、日期分布几年),不追求陷阱值;
// 执行计划用正常的设置(打开索引扫描)。每个声明式 API 跑两种请求: 只有必填参数 / 全部参数。
//
// 规则:
//   - 执行时间 > 阈值                         → ✗
//   - 排序溢出到磁盘(Sort Method: external)  → ✗
//   - 有必填的筛选条件,却对大表全表扫描        → ⚠(提示要加的索引)
package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

type Options struct {
	DB    string
	Rows  int     // 每张物理表的行数
	MaxMs float64 // 执行时间的阈值
}

type Report struct {
	API      string
	Variant  string // 必填参数 / 全部参数
	Ms       float64
	Rows     int // 返回的行数
	Fail     []string
	Warn     []string
	Plan     string // 计划的摘要(节点类型)
	Disabled string // 跳过的原因
	Boundary string // 已知边界(超时但原因是工厂的边界,不是回归)
}

// table: 一张物理表要生成的数据。
type table struct {
	name      string
	cols      []core.NamedType
	unique    map[string]bool
	composite map[string][2]int               // 组合主键/唯一约束的列 → (该列在约束里的位置, 约束的列数)
	json      map[string]map[string]core.Kind // JSON 列 → 键 → 类型
	nkKeys    map[string][2]int               // JSON 版本的自然键 → (位置, 列数)。按 i/5 混合进制拆开: 每个键约 5 个版本
	delKey    map[string]string               // 逻辑删除的键 → 值
	action    string                          // 操作列(删除值 1)
	orderTs   string                          // 登记顺序(时间)
	enums     map[string][]int64              // 枚举列(物理列名或 JSON 键) → 合法的 DB 值
}

func Run(ctx context.Context, plan *ir.Plan, opt Options) ([]Report, error) {
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
	tables := map[string]*table{}
	for i := range plan.APIs {
		api := &plan.APIs[i]
		if api.Pipeline == nil {
			continue
		}
		if err := collect(ctx, conn, plan, api, tables); err != nil {
			return nil, fmt.Errorf("%s: %w", api.ID, err)
		}
	}
	names := make([]string, 0, len(tables))
	for n := range tables {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := fill(ctx, conn, tables[n], opt.Rows); err != nil {
			return nil, fmt.Errorf("数据 %s: %w", n, err)
		}
	}
	var out []Report
	for i := range plan.APIs {
		api := &plan.APIs[i]
		if api.Pipeline == nil {
			out = append(out, Report{API: api.ID, Disabled: "逃生口 SQL / 自定义(没有流水线)"})
			continue
		}
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

	}
	return out, nil
}

// collect: 从流水线的来源部分知道要生成哪张物理表、哪些列 / JSON 的键。
func collect(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, api *ir.API, tables map[string]*table) error {
	scan := api.Pipeline.Steps[0]
	t, ok := tables[scan.Table]
	if !ok {
		t = &table{name: scan.Table, cols: scan.Columns, unique: map[string]bool{}, composite: map[string][2]int{}, json: map[string]map[string]core.Kind{},
			nkKeys: map[string][2]int{}, delKey: map[string]string{}, enums: map[string][]int64{}}
		rows, err := conn.Query(ctx, `
			SELECT kcu.column_name, count(*) OVER (PARTITION BY tc.constraint_name), kcu.ordinal_position
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
			WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')`, scan.Table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c string
			var n int64
			var pos int32
			if err := rows.Scan(&c, &n, &pos); err != nil {
				rows.Close()
				return err
			}
			if n == 1 {
				t.unique[c] = true
			} else if _, seen := t.composite[c]; !seen {
				t.composite[c] = [2]int{int(pos) - 1, int(n)}
			}
		}
		rows.Close()
		tables[scan.Table] = t
	} else { // 别的 API 读同一张表的其他列
		for _, c := range scan.Columns {
			if !slices.ContainsFunc(t.cols, func(x core.NamedType) bool { return x.Name == c.Name }) {
				t.cols = append(t.cols, c)
			}
		}
	}
	st := plan.Storage(api.Table)
	if st == nil {
		for _, c := range t.cols {
			if en := plan.Enum(api.Table, c.Name); en != nil {
				t.enums[c.Name] = enumValues(en)
			}
		}
		return nil
	}
	t.action, t.orderTs = st.ActionColumn, ""
	for _, c := range st.OrderBy {
		for _, x := range t.cols {
			if x.Name == c && x.Type.Kind == core.Timestamp {
				t.orderTs = c
			}
		}
	}
	if st.LogicalDeleteKey != "" {
		t.delKey[st.LogicalDeleteKey] = st.LogicalDeleteValue
	}
	keys := map[string]core.Kind{}
	for _, f := range st.Fields {
		if k, ok := kindOfSQL(f.Type); ok {
			keys[f.Key] = k
		}
		if en := plan.Enum(api.Table, f.Name); en != nil {
			t.enums[f.Key] = enumValues(en)
		}
		for pos, nk := range st.NaturalKey {
			if nk == f.Name {
				t.nkKeys[f.Key] = [2]int{pos, len(st.NaturalKey)}
			}
		}
	}
	t.json[st.DataColumn] = keys
	return nil
}

func enumValues(en *ir.Enum) []int64 {
	var out []int64
	for _, v := range en.Values {
		out = append(out, v.DB)
	}
	return out
}

// enumSQL: 在合法值里轮流取。
func enumSQL(vals []int64, salt int) string {
	var xs []string
	for _, v := range vals {
		xs = append(xs, fmt.Sprint(v))
	}
	return fmt.Sprintf("(ARRAY[%s])[1 + (i + %d) %% %d]", strings.Join(xs, ","), salt, len(vals))
}

func kindOfSQL(s string) (core.Kind, bool) {
	switch s {
	case "int4", "integer", "int2", "smallint", "serial":
		return core.Int32, true
	case "int8", "bigint", "bigserial":
		return core.Int64, true
	case "text", "varchar":
		return core.Text, true
	case "bool", "boolean":
		return core.Bool, true
	case "numeric":
		return core.Decimal, true
	case "date":
		return core.Date, true
	case "timestamptz", "timestamp":
		return core.Timestamp, true
	}
	return "", false
}

// valueSQL: 第 i 行(SQL 表达式里的 i)这一列的值。salt 让不同的列不相关。
func valueSQL(k core.Kind, salt int, unique bool) string {
	h := fmt.Sprintf("((i::int8 * %d + %d) %% 1000)::int4", 7919+2*salt, salt*13)
	switch k {
	case core.Int32, core.Int64:
		if unique {
			return "i"
		}
		return h + " + 1"
	case core.Text:
		if unique {
			return "'k' || i"
		}
		return fmt.Sprintf("'v' || ((i::int8 * %d) %% 5000)", 104729+salt)
	case core.Bool:
		return fmt.Sprintf("(i + %d) %% 2 = 0", salt)
	case core.Decimal:
		return "(" + h + " * 1.25)::numeric"
	case core.Date:
		return "date '2024-01-01' + " + h
	case core.Timestamp:
		return "timestamptz '2024-01-01 00:00:00+00' + make_interval(mins => i)"
	}
	return "NULL"
}

// jsonSQL: JSON 里的值(和 json_field 的 cast 对应: 日期 / 十进制 / 时间是字符串)。
func jsonSQL(k core.Kind, salt int, nk [2]int, n int) string {
	if nk[1] > 0 { // 自然键: 行号 / 5 按混合进制拆到各个键列(3 列的键每列约 34 种值),每个键约 5 个版本
		base := mixedRadix("(i / 5)", nk, n/5)
		switch k {
		case core.Text:
			return "'k' || " + base
		case core.Date:
			return "to_char(date '2024-01-01' + " + base + ", 'YYYY-MM-DD')"
		case core.Timestamp:
			return "to_char(timestamptz '2024-01-01 00:00:00+00' + make_interval(mins => " + base + ") AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
		case core.Decimal:
			return base + "::text"
		case core.Bool:
			return base + " % 2 = 0"
		default:
			return base + " + 1"
		}
	}
	v := valueSQL(k, salt, false)
	switch k {
	case core.Date:
		return "to_char(" + v + ", 'YYYY-MM-DD')"
	case core.Decimal:
		return "(" + v + ")::text"
	case core.Timestamp:
		return "to_char(" + v + " AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
	}
	return v
}

func fill(ctx context.Context, conn *pgx.Conn, t *table, n int) error {
	ident := pgx.Identifier{t.name}.Sanitize()
	if _, err := conn.Exec(ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
		return err
	}
	var cols, exprs []string
	for salt, c := range t.cols {
		cols = append(cols, pgx.Identifier{c.Name}.Sanitize())
		var e string
		switch {
		case c.Type.Kind == core.JSON:
			keys := t.json[c.Name]
			names := make([]string, 0, len(keys))
			for k := range keys {
				names = append(names, k)
			}
			sort.Strings(names)
			var parts []string
			for j, k := range names {
				v := jsonSQL(keys[k], salt*31+j, t.nkKeys[k], n)
				if en, ok := t.enums[k]; ok {
					v = enumSQL(en, j)
				}
				parts = append(parts, quote(k), v)
			}
			for k, v := range t.delKey { // 逻辑删除: 1%
				parts = append(parts, quote(k), fmt.Sprintf("CASE WHEN i %% 100 = 0 THEN %s ELSE '0' END", quote(v)))
			}
			e = "jsonb_build_object(" + strings.Join(parts, ", ") + ")"
		case c.Name == t.action: // 删除: 2%
			e = "CASE WHEN i % 50 = 0 THEN 1 ELSE 0 END"
		case len(t.enums[c.Name]) > 0:
			e = enumSQL(t.enums[c.Name], salt)
		case c.Name == t.orderTs:
			e = "timestamptz '2024-01-01 00:00:00+00' + make_interval(secs => i)"
		case !t.unique[c.Name] && t.composite[c.Name][1] > 1:
			e = compositeSQL(c.Type.Kind, t.composite[c.Name], n)
		default:
			e = valueSQL(c.Type.Kind, salt, t.unique[c.Name])
		}
		exprs = append(exprs, e)
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM generate_series(1, %d) AS i", ident, strings.Join(cols, ", "), strings.Join(exprs, ", "), n)
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%w\n%s", err, sql)
	}
	_, err := conn.Exec(ctx, "ANALYZE "+ident)
	return err
}

// compositeSQL: 组合键的第 pos 列。把行号按混合进制拆开(每列约 n^(1/k) 种值),元组就唯一。
func compositeSQL(k core.Kind, pk [2]int, n int) string {
	base := mixedRadix("i", pk, n)
	switch k {
	case core.Text:
		return "'k' || " + base
	case core.Date:
		return "date '2024-01-01' + " + base
	case core.Timestamp:
		return "timestamptz '2024-01-01 00:00:00+00' + make_interval(mins => " + base + ")"
	case core.Decimal:
		return base + "::numeric"
	}
	return base + " + 1"
}

// mixedRadix: 把 idx(0..total)按混合进制拆开,取第 pos 位(共 cnt 位,最后一位不取模)。各位组合起来唯一。
func mixedRadix(idx string, pk [2]int, total int) string {
	pos, cnt := pk[0], pk[1]
	radix := max(2, int(math.Ceil(math.Pow(float64(total), 1/float64(cnt)))))
	div := 1
	for j := 0; j < pos; j++ {
		div *= radix
	}
	if pos == cnt-1 {
		return fmt.Sprintf("(%s / %d)", idx, div)
	}
	return fmt.Sprintf("((%s / %d) %% %d)", idx, div, radix)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sample: 数据里这一列(逻辑列: JSON 版本取 data->>键)按顺序排在 q 分位的值。
// 区间筛选的下界取 0.4、上界取 0.6,区间里约有 20% 的行;其他取中位数。
func sample(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, api *ir.API, col string, k core.Kind, n int, q float64) (any, error) {
	scan := api.Pipeline.Steps[0]
	expr := pgx.Identifier{col}.Sanitize()
	if st := plan.Storage(api.Table); st != nil {
		for _, f := range st.Fields {
			if f.Name == col {
				expr = fmt.Sprintf("(%s ->> %s)::%s", pgx.Identifier{st.DataColumn}.Sanitize(), quote(f.Key), k.SQLType())
			}
		}
	}
	var s *string
	err := conn.QueryRow(ctx, fmt.Sprintf("SELECT %s::text FROM %s WHERE %s IS NOT NULL ORDER BY 1 OFFSET %d LIMIT 1",
		expr, pgx.Identifier{scan.Table}.Sanitize(), expr, int(float64(n)*q))).Scan(&s)
	if err != nil {
		return nil, err
	}
	return core.FromPGText(s, k)
}

// quantile: 筛选的种类 → 取样的分位。
func quantile(op string) float64 {
	switch op {
	case "gt", "gte", "大于", "大于等于":
		return 0.4
	case "lt", "lte", "小于", "小于等于":
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
	filters := map[string]bool{}
	for _, f := range api.Filters {
		filters[f.Field] = true
	}
	var out []string
	for _, d := range q.Params {
		if filters[d.Name] && !required(api, d) {
			out = append(out, d.Name)
		}
	}
	return out, nil
}

// explain: variant = "" 只有必填参数 / "*" 全部参数 / 参数名 必填参数 + 这一个可选参数。
func explain(ctx context.Context, conn *pgx.Conn, plan *ir.Plan, api *ir.API, variant string, opt Options, tables map[string]*table) (Report, error) {
	all := variant == "*"
	r := Report{API: api.ID, Variant: "必填参数"}
	switch variant {
	case "":
	case "*":
		r.Variant = "全部参数"
	default:
		r.Variant = "+" + variant
	}
	q, err := core.ToSQL(api.Pipeline, core.Options{})
	if err != nil {
		return r, err
	}
	filters := map[string]ir.Filter{}
	for _, f := range api.Filters {
		filters[f.Field] = f
	}
	var args []any
	var requiredCols, givenCols []string
	for _, d := range q.Params {
		var v any
		switch {
		case d.Name == "row_limit" && api.Keyset != nil:
			v = int64(api.Keyset.DefaultSize + 1)
		case strings.HasPrefix(d.Name, "cursor_"):
			v = nil // 第一页
		default:
			f, ok := filters[d.Name]
			required := required(api, d)
			if !ok || (!required && !all && d.Name != variant) {
				break
			}
			if required && !slices.Contains(requiredCols, f.Column) {
				requiredCols = append(requiredCols, f.Column)
			}
			x, err := sample(ctx, conn, plan, api, f.Column, d.Type.Kind, opt.Rows, quantile(f.Op))
			if err != nil {
				return r, fmt.Errorf("参数 %s: %w", d.Name, err)
			}
			if s, isText := x.(string); isText && (f.Op == "prefix" || f.Op == "前缀匹配") {
				x = string([]rune(s)[:min(2, len([]rune(s)))])
			}
			v = x
			givenCols = append(givenCols, f.Column)
			if d.List {
				v = []any{x}
			}
		}
		args = append(args, sqlArg(v))
	}
	var raw string
	start := time.Now()
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+q.Text, args...).Scan(&raw); err != nil {
		return r, fmt.Errorf("explain: %w\n%s", err, q.Text)
	}
	_ = start
	var doc []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) == 0 {
		return r, fmt.Errorf("explain json: %v", err)
	}
	r.Ms = doc[0].ExecutionTime
	if v, ok := doc[0].Plan["Actual Rows"].(float64); ok {
		r.Rows = int(v)
	}
	var nodes []string
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		typ, _ := n["Node Type"].(string)
		rel, _ := n["Relation Name"].(string)
		label := typ
		if rel != "" {
			label += "(" + rel + ")"
		}
		if idx, _ := n["Index Name"].(string); idx != "" {
			label += "[" + idx + "]"
		}
		nodes = append(nodes, label)
		if m, _ := n["Sort Method"].(string); strings.Contains(m, "external") {
			var keys []string
			if ks, ok := n["Sort Key"].([]any); ok {
				for _, k := range ks {
					keys = append(keys, fmt.Sprint(k))
				}
			}
			r.Fail = append(r.Fail, fmt.Sprintf("排序溢出到磁盘(%s);考虑按排序键建索引: %s", m, strings.Join(keys, ", ")))
		}
		if typ == "Seq Scan" && len(requiredCols) > 0 {
			if t, ok := tables[rel]; ok && opt.Rows > 10000 && t != nil {
				r.Warn = append(r.Warn, fmt.Sprintf("有必填的筛选条件却全表扫描 %s(%d 行);考虑索引 (%s)", rel, opt.Rows, strings.Join(requiredCols, ", ")))
			}
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(doc[0].Plan)
	r.Plan = strings.Join(nodes, " → ")
	if variant == "" && r.Rows == 0 {
		r.Warn = append(r.Warn, "只有必填参数时 0 行: 样本参数没有命中数据,这个计划可能不代表真实负载")
	}
	if r.Ms > opt.MaxMs {
		if b := versionScan(api, givenCols); b != "" {
			r.Boundary = fmt.Sprintf("执行时间 %.1fms > %.0fms: %s", r.Ms, opt.MaxMs, b)
		} else {
			r.Fail = append(r.Fail, fmt.Sprintf("执行时间 %.1fms > %.0fms", r.Ms, opt.MaxMs))
		}
	}
	return r, nil
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
