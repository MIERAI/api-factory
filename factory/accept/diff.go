package accept

// 差分测试: 随机数据 + 随机请求,被测服务器的响应 vs 核心代数的参考求值器(IR 里的流水线)。
//
// 数据在运行时生成并插入: 主键 / 唯一列(从数据库查)给唯一的值,其他列从很小的值域随机取
// (值域小 → 并列、命中、不命中自然出现)。违反约束的行被数据库拒绝,跳过。
// 然后从数据库读回实际的数据交给参考求值器 —— 约束不需要工厂去推断。
//
// 只测查询流水线: get_by_id(单条)、list_keyset(分页,逐页翻完)。计算模式的输出是 AI 的逻辑,由计算例子验证。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/big"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"apifactory/factory/constraints"
	"apifactory/factory/core"
	"apifactory/factory/exits"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
	"apifactory/factory/protoload"
	"apifactory/factory/sources"
	"apifactory/factory/spec"
)

const (
	diffRows     = 40
	diffRequests = 40
)

type diffEnv struct {
	ctx    context.Context
	conn   *pgx.Conn // 主库
	db     *dbs      // 主库 + 分库
	plan   *ir.Plan
	api    *ir.API
	table  string
	cols   []core.NamedType
	rnd    *rand.Rand
	rows   []core.Row
	rowDB  []*pgx.Conn          // 分库: 每一行在哪个库(期待值只用请求所在的库的行: 版本的选择在各个库里分别进行)
	filter map[string]ir.Filter // 参数名 → 筛选
	exit   *exits.Exit          // 出口(期待值的定义)
	input  protoreflect.MessageDescriptor
	json   map[string][]jsonKey // JSON 列 → 流水线里读到的键(json_field)
	out    core.Schema          // 流水线最后的列(响应的值按这里的类型表示)
	// anchors: 请求参数取值的来源 = 来源部分(scan / 来源单元)的输出。
	// 普通的表 = 物理行;JSON版本 = 展开后的逻辑行(筛选的列在物理表里不存在)
	anchors   []core.Row
	consts    map[core.Kind][]any            // 流水线里出现的常量(加进值域: 删除值、逻辑删除值等要能被随机数据碰到)
	domains   map[string]*constraints.Domain // 列 → 值域(筛选这一列的参数有约束时: 范围 / 格式 …,否则请求都不合法)
	keyCol    map[string]string              // JSON 的键 → 逻辑列名(json_field 投影成的列)
	constFor  map[string][]any               // 列(物理列或 JSON 的键投影成的列)→ 来源里和它比较的常量(删除值 / 逻辑删除值)
	nkCols    map[string]bool                // 多版本来源的自然键(值域扩大,逻辑行才多)
	nkSpace   int                            // 自然键原来的组合数
	scale     int                            // 数据量的倍数(覆盖不足时每次重试加倍: 条件严格的 API 需要更多的数据)
	pickCols  map[string]bool                // API 部分的每组取一条的键
	groupCols map[string]bool                // API 部分"每组取一条"的分组列: 值域缩小到 2 个,组里才有多个候选
}

type jsonKey struct {
	key  string
	kind core.Kind
}

// collect: 遍历流水线的表达式,收集 json_field 的键和常量。
func (e *diffEnv) collect() {
	e.json, e.consts = map[string][]jsonKey{}, map[core.Kind][]any{}
	var walk func(x *core.Expr)
	e.keyCol = map[string]string{}
	e.constFor, e.nkCols, e.groupCols, e.pickCols = map[string][]any{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	rawOf := map[string]string{} // 投影成的列 → 物理列(col(x) 的投影)
	for _, st := range e.api.Pipeline.Steps {
		for _, x := range st.Project {
			if x.Expr.Op == "col" {
				rawOf[x.Name] = x.Expr.Name
			}
		}
		if st.Kind == "pick_per_key" && !strings.HasPrefix(st.From, "来源 ") {
			for _, k := range st.Keys {
				e.groupCols[k] = true
				e.pickCols[k] = true
			}
		}
		if st.Kind == "pick_per_key" && strings.HasPrefix(st.From, "来源 ") {
			for _, k := range st.Keys {
				e.nkCols[k] = true
			}
		}
		// 来源部分的条件: cmp(列, 常量) → 这一列的数据要能碰到这个常量
		if st.Kind == "filter" && strings.HasPrefix(st.From, "来源 ") {
			var find func(x *core.Expr)
			find = func(x *core.Expr) {
				if x == nil {
					return
				}
				if x.Op == "cmp" && len(x.Args) == 2 && x.Args[0].Op == "col" && x.Args[1].Op == "const" && x.Args[1].Value != nil {
					name := x.Args[0].Name
					if raw, ok := rawOf[name]; ok {
						name = raw
					}
					e.constFor[name] = append(e.constFor[name], x.Args[1].Value)
				}
				for _, a := range x.Args {
					find(a)
				}
			}
			find(st.Pred)
		}
	}
	walk = func(x *core.Expr) {
		if x == nil {
			return
		}
		switch x.Op {
		case "json_field":
			if c := x.Args[0]; c.Op == "col" {
				// 同一个键可能有两种用法(例: f_del 既是逻辑删除的键(按文本比较),又是整数的字段):
				// 只生成一种值,按更严格的类型(非文本)—— 否则会生成整数字段读不了的文本,查询失败
				found := false
				for i, k := range e.json[c.Name] {
					if k.key == x.Name {
						found = true
						if k.kind == core.Text && x.Type.Kind != core.Text {
							e.json[c.Name][i].kind = x.Type.Kind
						}
					}
				}
				if !found {
					e.json[c.Name] = append(e.json[c.Name], jsonKey{x.Name, x.Type.Kind})
				}
			}
		case "const":
			if x.Value != nil && x.Type != nil {
				e.consts[x.Type.Kind] = append(e.consts[x.Type.Kind], x.Value)
			}
		}
		for _, a := range x.Args {
			walk(a)
		}
	}
	for _, st := range e.api.Pipeline.Steps {
		walk(st.Pred)
		for _, x := range st.Project {
			walk(x.Expr)
			if x.Expr.Op == "json_field" {
				e.keyCol[x.Expr.Name] = x.Name
			}
		}
	}
	// 必填的等于 / 属于条件的列、分库键的列: 值域缩小(请求固定了这些列,数据要集中在少数几个值上才有多行的结果)
	for _, d := range e.api.Pipeline.Params {
		f, ok := e.filter[d.Name]
		if !ok || !d.Required {
			continue
		}
		switch f.Op {
		case "eq", "in":
			e.groupCols[f.Column] = true
		}
	}
	if sh := e.api.Shard; sh != nil {
		e.groupCols[sh.Column] = true
	}
	// 参数的约束(范围 / 格式 …,factory/constraints)→ 被它筛选的列(多列单元的每一列)的值域
	e.domains = map[string]*constraints.Domain{}
	for _, f := range e.api.Filters {
		d := constraints.DomainOf(e.api.Request.Field(f.Field))
		if d == nil {
			continue
		}
		cols := f.Columns
		if len(cols) == 0 {
			cols = []string{f.Column}
		}
		for _, c := range cols {
			e.domains[c] = d
		}
	}
	// 自然键的组合数 < 数据行数 / 5 时才扩大(目标: 每个键平均约 5 个版本;复合键本来组合就多,不扩大)
	if len(e.nkCols) > 0 {
		space := 1
		for col := range e.nkCols {
			space *= e.domainSize(col)
		}
		e.nkSpace = space
	}
}

// invalid: 请求按 proto 的校验规则(protovalidate,和服务器同一套规则)是否不合法。
// 随机请求会违反输入之间的规则(例如 created_after < created_before),这时期待 INVALID_ARGUMENT。
func (e *diffEnv) invalid(req map[string]any) (bool, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	msg := dynamicpb.NewMessage(e.input)
	if err := protojson.Unmarshal(b, msg); err != nil {
		return false, err
	}
	err = protovalidate.Validate(msg)
	var verr *protovalidate.ValidationError
	if errors.As(err, &verr) {
		return true, nil
	}
	return false, err
}

func runDiff(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API) []Result {
	conn := d.main
	p := api.Pipeline
	ex, ok := exits.ByKind(api.Pattern) // 出口的注册表: 期待值 / 出口用例 / 覆盖条件由出口自己定义
	if p == nil || !ok || ex.DiffCase == nil {
		return nil
	}
	fail := func(format string, args ...any) []Result {
		return []Result{{Spec: api.ID, Suite: "diff", Case: "准备", Passed: false, Detail: fmt.Sprintf(format, args...)}}
	}
	h := fnv.New64a()
	h.Write([]byte(api.ID + os.Getenv("FACTORY_DIFF_SEED"))) // 换种子: 检查覆盖不是靠运气
	env := &diffEnv{ctx: ctx, conn: conn, db: d, plan: plan, api: api, table: p.Steps[0].Table, cols: p.Steps[0].Columns,
		rnd: rand.New(rand.NewSource(int64(h.Sum64()))), filter: map[string]ir.Filter{}, exit: ex}
	for _, f := range api.Filters {
		env.filter[f.Field] = f
	}
	files, err := protoload.Load()
	if err != nil {
		return fail("proto: %v", err)
	}
	md, err := protoload.FindMethod(files, api.ProtoPackage, api.Service, api.Method)
	if err != nil {
		return fail("proto: %v", err)
	}
	env.input = md.Input()
	env.collect()
	if env.out, err = core.Check(p); err != nil {
		return fail("流水线: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		return fail("%v", err)
	}
	// 生成数据 → 先用参考求值器预估覆盖条件 → 不满足就换派生种子重新生成(最多 5 次;种子确定,结果可复现)
	seed0 := int64(h.Sum64())
	var inserted, rejected, attempts int
	var reqs []core.Params
	for attempt := 0; attempt < 5; attempt++ {
		attempts = attempt + 1
		env.rnd = rand.New(rand.NewSource(seed0 + int64(attempt)*7919))
		env.scale = 1 << min(attempt, 3) // 1, 2, 4, 8, 8 倍
		env.rows, env.anchors = nil, nil
		if inserted, rejected, err = env.load(); err != nil {
			return fail("数据: %v", err)
		}
		if inserted < 2 {
			return fail("数据: 只插入了 %d 行(拒绝 %d 行)", inserted, rejected)
		}
		if err := env.loadAnchors(); err != nil {
			if attempt < 4 { // 来源部分没有行(数据都被删除了)→ 换种子重来
				continue
			}
			return fail("来源: %v", err)
		}
		reqs = env.requests()
		if env.planned(reqs) {
			break
		}
	}

	var results []Result
	hits := map[string]int{} // 覆盖: 0 行 / 1 行 / 多行 / 出口的标签
	for i, params := range reqs {
		name := fmt.Sprintf("#%02d %s", i+1, env.describe(params))
		ok, detail, n, tag := env.runOne(url, params)
		if tag != "" {
			hits[tag]++
		}
		switch {
		case n < 0:
			hits["invalid"]++
		case n == 0:
			hits["0"]++
		case n == 1:
			hits["1"]++
		default:
			hits["n"]++
		}
		results = append(results, Result{Spec: api.ID, Suite: "diff", Case: name, Passed: ok, Detail: detail})
	}
	if ex.DiffExtra != nil { // 出口自己的用例(分页: 途中改条件 / 非法 token …)
		for _, c := range ex.DiffExtra(env) {
			if c.Fail != "" {
				results = append(results, Result{Spec: api.ID, Suite: "diff", Case: c.Name, Passed: false, Detail: c.Fail})
				continue
			}
			ok, detail := runCase(url, c.Case, &api.Response)
			results = append(results, Result{Spec: api.ID, Suite: "diff", Case: c.Name, Passed: ok, Detail: detail})
		}
	}
	// 覆盖条件: 找到 / 找不到都出现;出口要求多行时(分页 / 多条)还要出现多行
	cover := fmt.Sprintf("数据 %d 行(拒绝 %d,来源 %d 行)/ 结果 0 行 %d 次、1 行 %d 次、多行 %d 次、不合法 %d 次", inserted, rejected, len(env.anchors), hits["0"], hits["1"], hits["n"], hits["invalid"])
	okCover := hits["0"] > 0 && hits["1"]+hits["n"] > 0
	if len(env.userParams()) == 0 { // 没有输入: 只有一种请求,"0 行"的情况不存在
		okCover = hits["1"]+hits["n"] > 0
	}
	if ex.DiffMulti && !env.atMostOneRow() {
		okCover = okCover && hits["n"] > 0
	}
	if ex.DiffCover != nil {
		cover += ex.DiffCover(api, hits)
	}
	// 多版本的来源(每键取一条): 多版本的键 / 被删除的键 都要 ≥ 3,否则版本的机制没有被测到
	if versions, removed, ok := env.versionCounts(); ok {
		cover += fmt.Sprintf(" / 来源: 多出来的旧版本 %d 行、最新是删除或逻辑删除的键 %d 个", versions, removed)
		okCover = okCover && versions >= 3 && removed >= 3
	}
	// 每组取一条(API 部分): 组里有 ≥2 个候选、真的淘汰掉了行的请求要 ≥ 3,否则"取最大还是最小"没有被测到
	if n, ok := env.pickReduced(reqs); ok {
		cover += fmt.Sprintf(" / 每组取一条淘汰了候选的请求 %d 个", n)
		okCover = okCover && n >= 3
	}
	if ex.DiffRowsNeed != nil { // 出口要的数据(分页: ≥5 行;前N条: 超过 N 行的请求 ≥3)
		minRows, need := ex.DiffRowsNeed(api)
		got := 0
		for _, ps := range reqs {
			if bad, err := env.invalid(env.request(ps)); err != nil || bad {
				continue
			}
			if n, err := env.rowsBeforeLimit(ps); err == nil && n >= minRows {
				got++
			}
		}
		cover += fmt.Sprintf(" / 结果 ≥%d 行的请求 %d 个(出口要 %d 个)", minRows, got, need)
		okCover = okCover && got >= need
	}
	if weak, total := env.sensitivity(reqs); total > 0 {
		cover += fmt.Sprintf(" / 起作用的条件 %d/%d", total-len(weak), total)
		if len(weak) > 0 {
			cover += fmt.Sprintf("(不足 3 个请求: %s)", strings.Join(weak, "、"))
			okCover = false
		}
	}
	if attempts > 1 {
		cover += fmt.Sprintf(" / 数据生成 %d 次(覆盖条件不满足时换种子、数据加倍重来;最后 %d 倍)", attempts, env.scale)
	}
	results = append(results, Result{Spec: api.ID, Suite: "diff", Case: "覆盖条件", Passed: okCover, Detail: cover})
	return results
}

// ---------------------------------------------------------------------------
// 数据
// ---------------------------------------------------------------------------

// atMostOneRow: 每个合法的请求最多返回 1 行(多版本来源的自然键全部被必填的"等于"固定)。
// 这时"多行的结果"不可能出现,出口的这个覆盖条件不要求。(普通的表的主键要查约束,还没有处理。)
func (e *diffEnv) atMostOneRow() bool {
	if len(e.nkCols) == 0 {
		return false
	}
	fixed := map[string]bool{}
	for _, d := range e.api.Pipeline.Params {
		f, ok := e.filter[d.Name]
		if ok && d.Required && !d.List && f.Op == "eq" && len(f.Columns) == 0 {
			fixed[f.Column] = true
		}
	}
	for c := range e.nkCols {
		if !fixed[c] {
			return false
		}
	}
	return true
}

// uniqueColumns: 需要唯一序号的列。单列的主键 / 唯一约束 → 这一列;
// 复合的 → 最后一列(前面的列保持小值域,形成分组: 例如 (order_id, line_no) 每个订单有多行)。
func (e *diffEnv) uniqueColumns() (map[string]bool, error) {
	rows, err := e.conn.Query(e.ctx, `
		SELECT kcu.column_name,
		       kcu.ordinal_position = max(kcu.ordinal_position) OVER (PARTITION BY tc.constraint_name)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
		WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')`, e.table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var col string
		var last bool
		if err := rows.Scan(&col, &last); err != nil {
			return nil, err
		}
		if last {
			out[col] = true
		}
	}
	return out, rows.Err()
}

func (e *diffEnv) value(c core.NamedType, i int, unique bool) any {
	if c.Type.Nullable && e.rnd.Intn(5) == 0 {
		return nil
	}
	if en := e.plan.Enum(e.table, c.Name); en != nil {
		v := en.Values[e.rnd.Intn(len(en.Values))]
		return v.DB
	}
	if c.Type.Kind == core.JSON { // 按流水线读到的键生成对象;有时缺少某个键
		obj := map[string]any{}
		for _, k := range e.json[c.Name] {
			if e.rnd.Intn(6) == 0 {
				continue
			}
			v := e.scalarFor(e.keyCol[k.key], k.kind, false, i)
			switch x := v.(type) {
			case *big.Rat:
				v = core.RatString(x)
			case time.Time:
				if k.kind == core.Date {
					v = x.Format("2006-01-02")
				} else {
					v = x.Format(time.RFC3339)
				}
			}
			obj[k.key] = v
		}
		b, _ := json.Marshal(obj)
		return string(b)
	}
	return e.scalarFor(c.Name, c.Type.Kind, unique, i)
}

// scalarFor: 某一列的值(这一列有范围值域时从里面取)。
func (e *diffEnv) scalarFor(col string, kind core.Kind, unique bool, i int) any {
	if e.groupCols[col] && !e.nkCols[col] && !unique { // 密集的列: 3 个值(多版本来源的自然键除外: 键要多样)
		// 每组取一条的键: 约 1/4 的行用独有的值(组里只有一行)。只看"组存不存在"的 API(只输出键)里,
		// 删除 / 逻辑删除只有在组里只剩这一行时才看得出来(试用: 按 code 去重的存在检查)
		if e.pickCols[col] && e.rnd.Intn(4) == 0 {
			if d, ok := e.domains[col]; ok && d.Expand != nil {
				return d.Expand(1000 + i)
			}
			if _, constrained := e.domains[col]; !constrained {
				if en := e.plan.Enum(e.table, col); en == nil {
					return e.scalar(kind, true, i)
				}
			}
		}
		if d, ok := e.domains[col]; ok {
			return d.Values[e.rnd.Intn(min(3, len(d.Values)))]
		}
		if t, ok := kinds.ByCore(kind); ok && len(t.Domain) >= 2 {
			return t.Domain[e.rnd.Intn(min(3, len(t.Domain)))] // 有约束的列走上面的 domains
		}
	}
	if cs := e.constFor[col]; len(cs) > 0 && !unique {
		if e.rnd.Intn(6) == 0 {
			return cs[e.rnd.Intn(len(cs))] // 来源里和这一列比较的常量(删除 / 逻辑删除要能出现)
		}
		// 其他时候不取这些常量(值域里本来就有删除值 1 的话,删除的概率会叠加)
		if t, ok := kinds.ByCore(kind); ok {
			var rest []any
			for _, v := range t.Domain {
				hit := false
				for _, c := range cs {
					hit = hit || core.Canonical(v) == core.Canonical(c)
				}
				if !hit {
					rest = append(rest, v)
				}
			}
			if len(rest) > 0 {
				return rest[e.rnd.Intn(len(rest))]
			}
		}
	}
	// 自然键的一部分又是每组取一条的键(组比自然键粗: 一个组里有很多条版本链): 约 1/4 的行用独有的值,
	// 这个组只有一条版本链,它的有效期 / 删除才看得出来(试用: 按 code 去重的存在检查,自然键 [code, 种别, 开始日])
	if e.nkCols[col] && e.pickCols[col] && !unique && e.rnd.Intn(4) == 0 {
		if d, ok := e.domains[col]; ok {
			if d.Expand != nil {
				return d.Expand(5000 + i)
			}
		} else if en := e.plan.Enum(e.table, col); en == nil {
			return e.scalar(kind, true, i)
		}
	}
	// 多版本来源的自然键: 键的数量 ≈ 数据行数 / 5(每个键约 5 个版本;数据加倍时键也加倍)
	target := diffRows * 3 / 5 * max(e.scale, 1)
	if e.nkCols[col] && e.nkSpace > 0 && e.nkSpace < target && !unique {
		times := (target + e.nkSpace - 1) / e.nkSpace
		if d, ok := e.domains[col]; ok && d.Expand != nil { // 有约束的列: 在约束里扩大
			return d.Expand(e.rnd.Intn(target))
		}
		if _, constrained := e.domains[col]; !constrained {
			v := e.scalar(kind, false, i)
			if e.rnd.Intn(3) == 0 { // 约 1/3 保留原来的值(陷阱值: "" / "a%" / 0 …): 扩大会把它们变成普通的值
				return v
			}
			switch v := v.(type) { // 多版本来源的自然键: 值域扩大
			case int64:
				return v + int64(100*e.rnd.Intn(times))
			case string:
				return fmt.Sprintf("%s%d", v, e.rnd.Intn(times))
			default:
				return v
			}
		}
	}
	if d, ok := e.domains[col]; ok && !unique {
		return d.Values[e.rnd.Intn(len(d.Values))]
	}
	return e.scalar(kind, unique, i)
}

func (e *diffEnv) scalar(kind core.Kind, unique bool, i int) any {
	if cs := e.consts[kind]; len(cs) > 0 && !unique && e.rnd.Intn(8) == 0 {
		return cs[e.rnd.Intn(len(cs))] // 流水线里的常量
	}
	t, ok := kinds.ByCore(kind) // 值域来自类型的注册表(factory/kinds)
	if !ok || len(t.Domain) == 0 {
		return nil
	}
	if _, isInt := t.Domain[0].(int64); isInt && unique { // 唯一列: 序号
		return int64(i + 1)
	}
	v := t.Domain[e.rnd.Intn(len(t.Domain))]
	if s, isText := v.(string); isText && unique {
		return fmt.Sprintf("%s-%02d", s, i)
	}
	return v
}

func sqlValue(v any) any {
	switch x := v.(type) {
	case *big.Rat:
		return core.RatString(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	}
	return v
}

// load: 生成 → 逐行插入(违反约束的行被拒绝)→ 读回实际的数据。
func (e *diffEnv) load() (int, int, error) {
	unique, err := e.uniqueColumns()
	if err != nil {
		return 0, 0, err
	}
	ident := pgx.Identifier{e.table}.Sanitize()
	sh := e.plan.Shard(e.api.Table)
	targets, err := e.db.targets(sh)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range targets {
		if _, err := c.Exec(e.ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
			return 0, 0, err
		}
	}
	// 分库键在物理行里的值: 普通的表 = 列;其他来源由来源决定(JSON版本 = JSON 里的键)
	st := e.plan.Storage(e.api.Table)
	var src *sources.Source
	if st != nil {
		src, _ = sources.ByKind(st.Kind)
	}
	var names, holders []string
	for i, c := range e.cols {
		names = append(names, pgx.Identifier{c.Name}.Sanitize())
		holders = append(holders, fmt.Sprintf("$%d::%s", i+1, c.Type.Kind.SQLType()))
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(names, ", "), strings.Join(holders, ", "))
	rejected := 0
	n := diffRows
	if len(e.json) > 0 { // 多版本的来源(JSON版本 等): 一个键有好几行,多生成一些
		n = diffRows * 3
	}
	n *= max(e.scale, 1)
	for i := 0; i < n; i++ {
		var args []any
		conn := e.conn
		physical := map[string]any{}
		for _, c := range e.cols {
			v := e.value(c, i, unique[c.Name])
			physical[c.Name] = v
			args = append(args, sqlValue(v))
		}
		if sh != nil { // 分库: 写进分库键对应的库
			key := physical[sh.Column]
			if src != nil && src.ShardKey != nil {
				key = src.ShardKey(st, sh.Column, physical)
			}
			if conn, err = e.db.forKey(key); err != nil {
				return 0, 0, err
			}
		}
		if _, err := conn.Exec(e.ctx, insert, args...); err != nil {
			rejected++
		}
	}
	var sel []string
	for _, c := range e.cols {
		sel = append(sel, pgx.Identifier{c.Name}.Sanitize()+"::text")
	}
	e.rowDB = nil
	for _, conn := range targets { // 读回: 所有的库合在一起(记下每一行在哪个库)
		rows, err := conn.Query(e.ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(sel, ", "), ident))
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			vals := make([]*string, len(e.cols))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return 0, 0, err
			}
			r := core.Row{}
			for i, c := range e.cols {
				v, err := core.FromPGText(vals[i], c.Type.Kind)
				if err != nil {
					rows.Close()
					return 0, 0, err
				}
				r[c.Name] = v
			}
			e.rows = append(e.rows, r)
			e.rowDB = append(e.rowDB, conn)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, 0, err
		}
	}
	return len(e.rows), rejected, nil
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

func (e *diffEnv) userParams() []core.ParamDecl {
	var out []core.ParamDecl
	for _, d := range e.api.Pipeline.Params {
		// 必填: 流水线的 Required(sqlc.arg)或 proto 的校验规则 required(时间是 message,流水线里看起来是可选的)
		if fd := e.api.Request.Field(d.Name); fd != nil && fd.Rules != nil && fd.Rules.Required {
			d.Required = true
		}
		if _, ok := e.filter[d.Name]; ok {
			out = append(out, d)
		}
	}
	return out
}

func missing(k core.Kind) any {
	if t, ok := kinds.ByCore(k); ok {
		return t.Missing
	}
	return nil
}

// requests: 每个请求以一行数据为"锚"(命中),每个参数随机取: 锚的值 / 另一行的值 / 不存在的值 / 不传(可选时)。
func (e *diffEnv) requests() []core.Params {
	var out []core.Params
	seen := map[string]bool{}
	for i := 0; len(out) < diffRequests && i < diffRequests*4; i++ {
		anchor := e.anchors[e.rnd.Intn(len(e.anchors))]
		other := e.anchors[e.rnd.Intn(len(e.anchors))]
		ps := core.Params{}
		for _, d := range e.userParams() {
			f := e.filter[d.Name]
			if d.List { // 列表参数: 0~3 个值(锚的值 / 别的行的值 / 不存在的值);必填至少 1 个
				n := e.rnd.Intn(4)
				if i == 0 && !d.Required {
					continue
				}
				if d.Required && n == 0 {
					n = 1
				}
				l := []any{}
				for j := 0; j < n; j++ {
					var v any
					switch {
					case j == 0 && e.rnd.Intn(4) > 0: // 1/4 的机会不放锚的值(可能 0 行)
						v = anchor[f.Column]
					case j == 0:
						v = e.missingFor(f.Column, d.Type.Kind)
					case e.rnd.Intn(3) == 0:
						v = e.missingFor(f.Column, d.Type.Kind)
					default:
						v = e.anchors[e.rnd.Intn(len(e.anchors))][f.Column]
					}
					if v != nil {
						l = append(l, v)
					}
				}
				if d.Required && len(l) == 0 {
					l = append(l, e.missingFor(f.Column, d.Type.Kind))
				}
				ps[d.Name] = l
				continue
			}
			choice := e.rnd.Intn(4)
			if i == 0 { // 第一个: 可选参数全部不传
				choice = 3
			}
			if choice == 3 && d.Required {
				choice = 0
			}
			var v any
			switch choice {
			case 0:
				v = anchor[f.Column]
			case 1:
				v = other[f.Column]
			case 2:
				if e.plan.Enum(e.api.Table, f.Column) != nil {
					v = other[f.Column] // 枚举: 只用定义过的值(未定义的值被请求校验拒绝)
				} else {
					v = e.missingFor(f.Column, d.Type.Kind)
				}
			case 3:
				continue
			}
			if v == nil {
				if d.Required {
					v = e.missingFor(f.Column, d.Type.Kind)
				} else {
					continue
				}
			}
			if f.Op == "prefix" {
				s := []rune(v.(string))
				cut := e.rnd.Intn(len(s) + 1)
				// 陷阱前缀: 有一半的机会截在 % 或 _ 之后(它们必须按字面匹配,不是通配符)
				for j, r := range s {
					if (r == '%' || r == '_') && e.rnd.Intn(2) == 0 {
						cut = j + 1
						break
					}
				}
				v = string(s[:cut])
				if v == "" && !d.Required {
					continue
				}
			}
			ps[d.Name] = v
		}
		key := e.describe(ps)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ps)
	}
	out = append(out, e.sweep(seen)...)
	return append(out, e.directed(out, seen)...)
}

func (e *diffEnv) describe(ps core.Params) string {
	var keys []string
	for k := range ps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+core.Canonical(ps[k]))
	}
	if len(parts) == 0 {
		return "(不传可选条件)"
	}
	return strings.Join(parts, " ")
}

// present: 核心的值 → 请求 / 期待值里的表示(与 protojson 一致)。column 用来查枚举。
func (e *diffEnv) present(column string, v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = e.present(column, y)
		}
		return out
	case time.Time:
		if t, _ := e.colType(column); t.Kind == core.Date {
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case *big.Rat:
		return core.RatString(x)
	case int64:
		if en := e.plan.Enum(e.api.Table, column); en != nil {
			for _, ev := range en.Values {
				if ev.DB == x {
					return ev.Name
				}
			}
			return en.Unspecified
		}
	}
	return v
}

// colType: 输出 / 筛选的列的类型(先看流水线的输出,再看扫描的表)
func (e *diffEnv) colType(name string) (core.Type, bool) {
	if t, ok := e.out.Col(name); ok {
		return t, true
	}
	return core.Schema(e.cols).Col(name)
}

func (e *diffEnv) request(ps core.Params) map[string]any {
	req := map[string]any{}
	for k, v := range ps {
		req[k] = e.present(e.filter[k].Column, v)
	}
	return req
}

func (e *diffEnv) item(r core.Row, msg *ir.Message) map[string]any {
	out := map[string]any{}
	for _, f := range msg.Fields {
		v := e.present(f.Name, r[f.Name])
		if v == nil {
			if f.Presence {
				out[f.Name] = "$absent"
			}
			continue
		}
		out[f.Name] = v
	}
	return out
}

// runOne: 一个请求。返回 (通过, 说明, 参考求值器的结果行数)。
func (e *diffEnv) runOne(url string, ps core.Params) (bool, string, int, string) {
	rows, err := e.Eval(ps)
	if errors.Is(err, core.ErrAmbiguous) {
		return false, "参考求值器: " + err.Error() + "(排序 / 键不唯一,设计的问题)", 0, ""
	}
	if err != nil {
		return false, "参考求值器: " + err.Error(), 0, ""
	}
	n := len(rows)
	req := e.request(ps)
	bad, err := e.invalid(req)
	if err != nil {
		return false, "请求校验: " + err.Error(), n, ""
	}
	if bad { // 不合法的请求: 期待 INVALID_ARGUMENT(不计入覆盖)
		ok, detail := runCase(url, spec.Case{Request: req, Expect: spec.Expect{Code: "INVALID_ARGUMENT"}}, &e.api.Response)
		return ok, "不合法的请求: " + detail, -1, ""
	}
	c, note, tag := e.exit.DiffCase(e, rows, req) // 期待值由出口决定
	if tag == "error" {
		return false, note, n, ""
	}
	ok, detail := runCase(url, c, &e.api.Response)
	if note != "" {
		detail = note + ": " + detail
	}
	return ok, detail, n, tag
}

// ---- exits.DiffCtx(出口定义期待值时用)

func (e *diffEnv) API() *ir.API                                    { return e.api }
func (e *diffEnv) Rand() *rand.Rand                                { return e.rnd }
func (e *diffEnv) Item(r core.Row, msg *ir.Message) map[string]any { return e.item(r, msg) }
func (e *diffEnv) Request(ps core.Params) map[string]any           { return e.request(ps) }
func (e *diffEnv) Requests() []core.Params                         { return e.requests() }
func (e *diffEnv) Invalid(req map[string]any) (bool, error)        { return e.invalid(req) }
func (e *diffEnv) UserParams() []core.ParamDecl                    { return e.userParams() }
func (e *diffEnv) Column(param string) string                      { return e.filter[param].Column }
func (e *diffEnv) Anchors() []core.Row                             { return e.anchors }
func (e *diffEnv) Present(column string, v any) any                { return e.present(column, v) }

// Eval: 参考求值器(参数 + 出口另外的参数,例如分页的 row_limit)。
func (e *diffEnv) Eval(ps core.Params) ([]core.Row, error) {
	evalParams := core.Params{}
	for k, v := range ps {
		evalParams[k] = v
	}
	if e.exit.DiffParams != nil {
		for k, v := range e.exit.DiffParams(e.api) {
			evalParams[k] = v
		}
	}
	res, err := core.Eval(e.api.Pipeline, map[string][]core.Row{e.table: e.rowsFor(ps)}, evalParams)
	if err != nil {
		return nil, err
	}
	return res.Rows, nil
}

// rowsFor: 请求能看到的行。分库的 API = 请求的分库键对应的那个库的行;否则全部。
func (e *diffEnv) rowsFor(ps core.Params) []core.Row {
	sh := e.api.Shard
	if sh == nil {
		return e.rows
	}
	conn, err := e.db.forKey(ps[sh.Field])
	if err != nil {
		return nil
	}
	var out []core.Row
	for i, r := range e.rows {
		if e.rowDB[i] == conn {
			out = append(out, r)
		}
	}
	return out
}

// loadAnchors: 只跑流水线的来源部分(编译器标记为 datasource.table / 来源 … 的步骤)。
func (e *diffEnv) loadAnchors() error {
	src := &core.Pipeline{Params: e.api.Pipeline.Params}
	for _, st := range e.api.Pipeline.Steps {
		if st.From != "datasource.table" && !strings.HasPrefix(st.From, "来源 ") {
			break
		}
		src.Steps = append(src.Steps, st)
	}
	if len(src.Steps) <= 1 {
		e.anchors = e.rows
		return nil
	}
	res, err := e.evalPerDB(src)
	if err != nil {
		return err
	}
	e.anchors = res.Rows
	if len(e.anchors) == 0 {
		return fmt.Errorf("来源部分没有行(数据都被删除了?)")
	}
	return nil
}

// missingFor: 数据里不存在的值(带格式的列用这个类型的,否则请求会被格式校验拒绝)。
func (e *diffEnv) missingFor(col string, k core.Kind) any {
	if d, ok := e.domains[col]; ok && d.Missing != nil {
		return d.Missing
	}
	return missing(k)
}

// trapPrefixes: 前缀匹配的陷阱(% _ 在开头 / 中间 / 结尾)。按字面匹配时几乎不命中,当成通配符时会命中很多行
var trapPrefixes = []string{"%", "a%", "%a", "_", "a_", "_b"}

// sweep: 确定性的扫值请求(不消耗随机数)。每个筛选参数单独取它的列在来源里出现的值(最多 5 个)+ 不存在的值;
// 前缀参数取陷阱前缀。必填的其他参数取自同一行(这样请求能命中)。陷阱值一定会被每个运算符碰到,不靠随机。
func (e *diffEnv) sweep(seen map[string]bool) []core.Params {
	var out []core.Params
	add := func(ps core.Params) {
		if k := e.describe(ps); !seen[k] {
			seen[k] = true
			out = append(out, ps)
		}
	}
	params := e.userParams()
	add(e.widest(params))
	stale := e.staleRows()
	removed := e.removedRows()
	for _, d := range params {
		f := e.filter[d.Name]
		// 这一列在来源里出现的值(规范文本排序,确定)
		byVal := map[string]core.Row{}
		valOf := map[string]any{}
		var keys []string
		cols := f.Columns // 多列的单元(有效期包含): 每一列的值都要扫(结束日的边界)
		if len(cols) == 0 {
			cols = []string{f.Column}
		}
		addVal := func(v any, r core.Row) {
			if v == nil {
				return
			}
			k := core.Canonical(v)
			if _, ok := byVal[k]; !ok {
				byVal[k], valOf[k] = r, v
				keys = append(keys, k)
			}
		}
		for _, c := range cols {
			for _, r := range e.anchors {
				addVal(r[c], r)
				if len(cols) > 1 { // 边界的另一侧: 值 + 1(结束日的下一天)
					switch x := r[c].(type) {
					case int64:
						addVal(x+1, r)
					case time.Time:
						addVal(x.AddDate(0, 0, 1), r)
					}
				}
			}
		}
		sort.Strings(keys)
		if limit := 5 * len(cols); len(keys) > limit {
			keys = keys[:limit]
		}
		// 多版本的来源: 只在旧版本里出现的值(用它筛选时,旧版本不能被返回: "先筛选再取最新"的陷阱)
		var staleKeys []string
		staleVal, staleRow := map[string]any{}, map[string]core.Row{}
		for _, r := range stale {
			v := r[f.Column]
			if v == nil {
				continue
			}
			k := core.Canonical(v)
			if _, current := byVal[k]; current {
				continue
			}
			if _, ok := staleVal[k]; !ok {
				staleVal[k], staleRow[k] = v, r
				staleKeys = append(staleKeys, k)
			}
		}
		sort.Strings(staleKeys)
		if len(staleKeys) > 8 {
			staleKeys = staleKeys[:8]
		}
		withRequired := func(anchor core.Row, ps core.Params) core.Params {
			for _, o := range params {
				if o.Name == d.Name || !o.Required {
					continue
				}
				col := e.filter[o.Name].Column
				var v any
				if anchor != nil {
					v = anchor[col]
				}
				if v == nil {
					v = e.missingFor(col, o.Type.Kind)
				}
				if o.List {
					v = []any{v}
				}
				ps[o.Name] = v
			}
			return ps
		}
		// 取最新之后被来源的每一项条件(删除 / 逻辑删除 …)淘汰的行: 每项各 3 行,用那一行本身发请求
		// (复合键时同一个值在别的组里可能还是现存的,所以不能只看"只在旧版本里出现的值")
		for _, set := range removed {
			n, tried := 0, 0
			// 优先: 这个值在现存的行里已经没有了(删除让它消失)
			rows := append([]core.Row(nil), set.rows...)
			sort.SliceStable(rows, func(i, j int) bool {
				_, ci := byVal[core.Canonical(rows[i][f.Column])]
				_, cj := byVal[core.Canonical(rows[j][f.Column])]
				return !ci && cj
			})
			// 只留下这一项条件真的影响结果的请求(用参考求值器验证: 被淘汰的行还要满足 API 的其他条件,
			// 例如有效期,否则不处理删除也看不出来)
			for _, r := range rows {
				v := r[f.Column]
				if v == nil {
					continue
				}
				if d.List {
					v = []any{v}
				}
				rq := withRequired(r, core.Params{d.Name: v})
				if tried++; tried > 60 {
					break
				}
				if bad, err := e.invalid(e.request(rq)); err != nil || bad || !e.sensitive(rq, set.step, set.node) {
					continue
				}
				add(rq)
				if n++; n == 3 {
					break
				}
			}
		}
		if f.Op == "prefix" {
			var anchor core.Row
			if len(keys) > 0 {
				anchor = byVal[keys[0]]
			}
			for _, p := range trapPrefixes {
				add(withRequired(anchor, core.Params{d.Name: p}))
			}
			continue
		}
		for _, k := range keys {
			r := byVal[k]
			var v any = valOf[k]
			if d.List {
				v = []any{v}
			}
			add(withRequired(r, core.Params{d.Name: v}))
			// 同一行的全部参数(组合条件也要碰到每个值)
			all := core.Params{}
			for _, o := range params {
				ov := r[e.filter[o.Name].Column]
				if ov == nil || e.filter[o.Name].Op == "prefix" {
					continue
				}
				if o.List {
					ov = []any{ov}
				}
				all[o.Name] = ov
			}
			add(withRequired(r, all))
			// 两两组合: 这个值 + 同一行的另一个参数(最多 2 个;全部参数一起时可能违反输入之间的规则)
			pairs := 0
			for _, o := range params {
				if o.Name == d.Name || pairs == 2 {
					continue
				}
				ov, ok := all[o.Name]
				if !ok {
					continue
				}
				add(withRequired(r, core.Params{d.Name: v, o.Name: ov}))
				pairs++
			}
		}
		if d.List && len(keys) > 1 { // 列表: 多个值一起(属于的多值命中)
			var vs []any
			for _, k := range keys {
				vs = append(vs, valOf[k])
			}
			add(withRequired(byVal[keys[0]], core.Params{d.Name: vs}))
		}
		if len(cols) > 1 { // 多列单元的边界: 以每一行自己的结束列为基准(= 结束日 / 结束日 + 1),其他参数取自同一行
			last := cols[len(cols)-1]
			for i, r := range e.anchors {
				if i == 20 {
					break
				}
				var next any
				switch x := r[last].(type) {
				case int64:
					next = x + 1
				case time.Time:
					next = x.AddDate(0, 0, 1)
				default:
					continue
				}
				for _, v := range []any{r[last], next} {
					add(withRequired(r, core.Params{d.Name: v}))
				}
			}
		}
		for _, k := range staleKeys {
			var v any = staleVal[k]
			if d.List {
				v = []any{v}
			}
			add(withRequired(staleRow[k], core.Params{d.Name: v})) // 其他必填参数取自同一行旧版本
		}
		var m any = e.missingFor(f.Column, d.Type.Kind)
		if d.List {
			m = []any{m}
		}
		var anchor core.Row
		if len(keys) > 0 {
			anchor = byVal[keys[0]]
		}
		add(withRequired(anchor, core.Params{d.Name: m}))
	}
	return out
}

// Missing: 参数的第 i 个"数据里不存在的值"。整数 +i,文本加后缀;带格式的类型只有一个(i > 0 → nil)。
func (e *diffEnv) Missing(param string, i int) any {
	var d *core.ParamDecl
	for _, x := range e.userParams() {
		if x.Name == param {
			x := x
			d = &x
		}
	}
	if d == nil {
		return nil
	}
	col := e.filter[param].Column
	base := e.missingFor(col, d.Type.Kind)
	if i == 0 {
		return base
	}
	// 带格式的文本有专门的"不存在的值"时只有这一个(加后缀会违反格式);
	// 整数(范围的"不存在的值"在扩大的区间之外)往上加仍然合法,照常 +1、+2
	if d, ok := e.domains[col]; ok && d.Missing != nil {
		if _, isInt := d.Missing.(int64); !isInt {
			return nil
		}
	}
	switch v := base.(type) {
	case int64:
		return v + int64(i)
	case string:
		return fmt.Sprintf("%s-%d", v, i)
	case time.Time:
		return v.AddDate(0, 0, i)
	}
	return nil
}

// versionCounts: 来源部分里"每键取一条"之前 / 之后 / 来源的最后,各有几行(参考求值器)。
// versions = 被取最新淘汰的旧版本数;removed = 最新一行被删除 / 逻辑删除的键数。没有每键取一条 → ok = false。
func (e *diffEnv) versionCounts() (versions, removed int, ok bool) {
	var src []core.Step
	pick := -1
	for _, st := range e.api.Pipeline.Steps {
		if st.From != "datasource.table" && !strings.HasPrefix(st.From, "来源 ") {
			break
		}
		if st.Kind == "pick_per_key" && pick < 0 {
			pick = len(src)
		}
		src = append(src, st)
	}
	if pick < 0 {
		return 0, 0, false
	}
	count := func(steps []core.Step) int {
		res, err := e.evalPerDB(&core.Pipeline{Steps: steps, Params: e.api.Pipeline.Params})
		if err != nil {
			return -1
		}
		return len(res.Rows)
	}
	before, keys, alive := count(src[:pick]), count(src[:pick+1]), count(src)
	return before - keys, keys - alive, true
}

// removedRows: 多版本来源里,每个键的最新版本被"取最新之后"的每一项条件(删除 / 逻辑删除 …)淘汰的行。
// 按条件分开(每一项各自要被请求碰到),行按规范文本排序(确定)。
type removedSet struct {
	step int
	node *core.Expr
	rows []core.Row
}

func (e *diffEnv) removedRows() []removedSet {
	var src []core.Step
	pick := -1
	for _, st := range e.api.Pipeline.Steps {
		if st.From != "datasource.table" && !strings.HasPrefix(st.From, "来源 ") {
			break
		}
		if st.Kind == "pick_per_key" && pick < 0 {
			pick = len(src)
		}
		src = append(src, st)
	}
	if pick < 0 {
		return nil
	}
	var out []removedSet
	for si, st := range src[pick+1:] {
		if st.Kind != "filter" || st.Pred == nil {
			continue
		}
		parts := []*core.Expr{st.Pred}
		if st.Pred.Op == "and" {
			parts = st.Pred.Args
		}
		for _, c := range parts {
			if !refersColumn(c) {
				continue
			}
			steps := append(append([]core.Step(nil), src[:pick+1]...), core.Filter(core.Not(c)))
			res, err := e.evalPerDB(&core.Pipeline{Steps: steps, Params: e.api.Pipeline.Params})
			if err != nil {
				continue
			}
			rows := res.Rows
			sort.Slice(rows, func(i, j int) bool { return fmt.Sprint(rows[i]) < fmt.Sprint(rows[j]) })
			out = append(out, removedSet{step: pick + 1 + si, node: c, rows: rows})
		}
	}
	return out
}

// staleRows: 多版本来源里"每键取一条"之前的行(包括被淘汰的旧版本)。没有每键取一条 → nil。
func (e *diffEnv) staleRows() []core.Row {
	var src []core.Step
	for _, st := range e.api.Pipeline.Steps {
		if st.From != "datasource.table" && !strings.HasPrefix(st.From, "来源 ") {
			break
		}
		if st.Kind == "pick_per_key" {
			res, err := e.evalPerDB(&core.Pipeline{Steps: src, Params: e.api.Pipeline.Params})
			if err != nil {
				return nil
			}
			return res.Rows
		}
		src = append(src, st)
	}
	return nil
}

// domainSize: 这一列的值域大小(范围 / 格式 / 枚举 / 类型的值域)。
func (e *diffEnv) domainSize(col string) int {
	if d, ok := e.domains[col]; ok {
		return len(d.Values)
	}
	if en := e.plan.Enum(e.api.Table, col); en != nil {
		return len(en.Values)
	}
	if t, ok := core.Schema(e.out).Col(col); ok {
		if k, ok := kinds.ByCore(t.Kind); ok && len(k.Domain) > 0 {
			return len(k.Domain)
		}
	}
	for _, k := range e.json { // JSON 的键: 投影后的列名找类型
		for _, x := range k {
			if e.keyCol[x.key] == col {
				if t, ok := kinds.ByCore(x.kind); ok && len(t.Domain) > 0 {
					return len(t.Domain)
				}
			}
		}
	}
	return 5
}

// planned: 用参考求值器预估这批请求的覆盖条件(不需要服务器)。和 runDiff 最后的覆盖条件一致。
func (e *diffEnv) planned(reqs []core.Params) bool {
	hits := map[string]int{}
	var sizes []int
	rowsAtLeast := func(n int) int {
		c := 0
		for _, x := range sizes {
			if x >= n {
				c++
			}
		}
		return c
	}
	for _, ps := range reqs {
		rows, err := e.Eval(ps)
		if err != nil {
			continue
		}
		if bad, err := e.invalid(e.request(ps)); err != nil || bad {
			continue
		}
		if n, err := e.rowsBeforeLimit(ps); err == nil {
			sizes = append(sizes, n)
		}
		switch n := len(rows); {
		case n == 0:
			hits["0"]++
		case n == 1:
			hits["1"]++
		default:
			hits["n"]++
		}
	}
	ok := hits["0"] > 0 && hits["1"]+hits["n"] > 0
	if len(e.userParams()) == 0 {
		ok = hits["1"]+hits["n"] > 0
	}
	if e.exit.DiffMulti && !e.atMostOneRow() {
		ok = ok && hits["n"] > 0
	}
	if e.exit.DiffRowsNeed != nil { // 出口要的数据(分页: ≥5 行;前N条: 超过 N 行的请求 ≥3)
		minRows, need := e.exit.DiffRowsNeed(e.api)
		ok = ok && rowsAtLeast(minRows) >= need
	}
	if versions, removed, has := e.versionCounts(); has {
		ok = ok && versions >= 3 && removed >= 3
	}
	if n, has := e.pickReduced(reqs); has {
		ok = ok && n >= 3
	}
	if weak, _ := e.sensitivity(reqs); len(weak) > 0 {
		ok = false
	}
	return ok
}

// sensitivity: 每个筛选条件(和条件里 and 的每一项)都要在 ≥3 个合法请求里起作用:
// 用参考求值器把它换成恒真,结果变了的请求数。不够 → 返回这些条件的说明(数据区分不出它们,测试会漏掉相应的 bug)。
// sensTarget: 条件的敏感度检查的对象(一个筛选,或筛选里 and 的一项)。
type sensTarget struct {
	step int
	node *core.Expr
	desc string
}

func (e *diffEnv) sensTargets() []sensTarget {
	p := e.api.Pipeline
	var targets []sensTarget
	picked := false // 来源里取最新版本之后的筛选(删除 / 逻辑删除)也要测到;之前的(键非空)不算
	for i, st := range p.Steps {
		source := st.From == "datasource.table" || strings.HasPrefix(st.From, "来源 ")
		if st.Kind == "pick_per_key" && source {
			picked = true
		}
		if st.Kind != "filter" || (source && !picked) || st.Pred == nil {
			continue
		}
		if st.Pred.Op == "tuple_after" { // 分页的游标: 由出口用例测
			continue
		}
		targets = append(targets, sensTarget{i, st.Pred, st.From})
		var walk func(x *core.Expr)
		walk = func(x *core.Expr) {
			if x == nil {
				return
			}
			if x.Op == "and" {
				for j, a := range x.Args {
					targets = append(targets, sensTarget{i, a, fmt.Sprintf("%s 的第 %d 项", st.From, j+1)})
				}
			}
			for _, a := range x.Args {
				walk(a)
			}
		}
		walk(st.Pred)
	}
	var out []sensTarget
	for _, t := range targets {
		if refersColumn(t.node) { // 不引用任何列的条件(没有逻辑删除列时的恒真项)不会让结果变化
			out = append(out, t)
		}
	}
	return out
}

// directed: 按条件的敏感度定向补请求。起作用的请求不到 3 个的条件,从数据的行(现存 + 旧版本)构造候选请求
// (每个参数取那一行的值;多列单元另外取结束日 / 结束日 + 1 / 开始日 − 1),用参考求值器验证"去掉这个条件结果会变",补到 3 个。
// 只看"存不存在"的 API(只输出键)里,随机的请求很难碰到"某个键在这个日期一个有效版本都没有"这类情况。
func (e *diffEnv) directed(reqs []core.Params, seen map[string]bool) []core.Params {
	var out []core.Params
	params := e.userParams()
	if len(params) == 0 {
		return nil
	}
	valid := []core.Params{}
	for _, ps := range reqs {
		if bad, err := e.invalid(e.request(ps)); err == nil && !bad {
			valid = append(valid, ps)
		}
	}
	rows := append(append([]core.Row(nil), e.anchors...), e.staleRows()...)
	if len(rows) > 300 {
		rows = rows[:300]
	}
	fromRow := func(r core.Row, shift int) core.Params {
		ps := core.Params{}
		for _, d := range params {
			f := e.filter[d.Name]
			col := f.Column
			if len(f.Columns) > 1 && (shift == 1 || shift == 2) { // 多列单元: 结束日(+1)
				col = f.Columns[len(f.Columns)-1]
			}
			v := r[col]
			if v == nil {
				v = r[f.Column]
			}
			if v == nil {
				if d.Required {
					v = e.missingFor(f.Column, d.Type.Kind)
				} else {
					continue
				}
			}
			if shift >= 2 && len(f.Columns) > 1 { // 2: 结束日 + 1 / 3: 开始日 − 1(早于这个键所有版本的开始日 → 键在这一天不存在)
				delta := 1
				if shift == 3 {
					delta = -1
				}
				switch x := v.(type) {
				case int64:
					v = x + int64(delta)
				case time.Time:
					v = x.AddDate(0, 0, delta)
				}
			}
			if d.List {
				v = []any{v}
			}
			ps[d.Name] = v
		}
		return ps
	}
	for _, t := range e.sensTargets() {
		n := 0
		for _, ps := range valid {
			if e.sensitive(ps, t.step, t.node) {
				if n++; n >= 3 {
					break
				}
			}
		}
		for _, r := range rows {
			if n >= 3 {
				break
			}
			for shift := 0; shift <= 3 && n < 3; shift++ {
				ps := fromRow(r, shift)
				k := e.describe(ps)
				if seen[k] {
					continue
				}
				if bad, err := e.invalid(e.request(ps)); err != nil || bad || !e.sensitive(ps, t.step, t.node) {
					continue
				}
				seen[k] = true
				out = append(out, ps)
				valid = append(valid, ps)
				n++
			}
		}
	}
	return out
}

func (e *diffEnv) sensitivity(reqs []core.Params) (weak []string, total int) {
	p := e.api.Pipeline
	targets := e.sensTargets()
	if len(targets) == 0 {
		return nil, 0
	}
	valid := []core.Params{}
	for _, ps := range reqs {
		if bad, err := e.invalid(e.request(ps)); err == nil && !bad {
			valid = append(valid, ps)
		}
	}
	eval := e.evalCanon
	base := map[int]string{}
	distinct := map[string]bool{}
	for i, ps := range valid {
		base[i] = eval(p, ps)
		b, _ := json.Marshal(ps)
		distinct[string(b)] = true
	}
	// 要求 ≥3 个请求;可能的请求本来就不到 3 种(例如没有输入的 API 只有 1 种)时,要求每一种
	need := min(3, len(distinct))
	for _, t := range targets {
		if !refersColumn(t.node) { // 不引用任何列的条件(例如没有逻辑删除列时的恒真项)不会让结果变化
			continue
		}
		mut := *p
		mut.Steps = append([]core.Step(nil), p.Steps...)
		mut.Steps[t.step].Pred = replaceExpr(p.Steps[t.step].Pred, t.node, core.Const(true, core.Type{Kind: core.Bool}))
		n := 0
		for i, ps := range valid {
			if eval(&mut, ps) != base[i] {
				n++
			}
		}
		if os.Getenv("FACTORY_DEBUG_SENS") != "" && len(valid) > 0 {
			fmt.Fprintf(os.Stderr, "SENS %s %s: n=%d base0=%.200q mut0=%.200q\n", e.api.ID, t.desc, n, base[0], eval(&mut, valid[0]))
		}
		total++
		if n < need {
			weak = append(weak, fmt.Sprintf("%s(%d 个请求)", t.desc, n))
		}
	}
	return weak, total
}

func refersColumn(x *core.Expr) bool {
	if x == nil {
		return false
	}
	if x.Op == "col" || x.Op == "tuple_after" {
		return true
	}
	for _, a := range x.Args {
		if refersColumn(a) {
			return true
		}
	}
	return false
}

// evalCanon: 参考求值器的结果(规范文本,比较用)。出错 → "error"。
func (e *diffEnv) evalCanon(pl *core.Pipeline, ps core.Params) string {
	params := core.Params{}
	for k, v := range ps {
		params[k] = v
	}
	if e.exit.DiffParams != nil {
		for k, v := range e.exit.DiffParams(e.api) {
			params[k] = v
		}
	}
	res, err := core.Eval(pl, map[string][]core.Row{e.table: e.rowsFor(ps)}, params)
	if err != nil {
		return "error"
	}
	var b strings.Builder
	for _, r := range res.Rows {
		for _, c := range res.Columns {
			b.WriteString(core.Canonical(r[c.Name]) + "|")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sensitive: 把第 step 步条件里的 node 换成恒真后,这个请求的结果变不变。
func (e *diffEnv) sensitive(ps core.Params, step int, node *core.Expr) bool {
	p := e.api.Pipeline
	mut := *p
	mut.Steps = append([]core.Step(nil), p.Steps...)
	mut.Steps[step].Pred = replaceExpr(p.Steps[step].Pred, node, core.Const(true, core.Type{Kind: core.Bool}))
	return e.evalCanon(&mut, ps) != e.evalCanon(p, ps)
}

// replaceExpr: 复制表达式树,把节点 target 换成 with(按指针)。
func replaceExpr(x, target, with *core.Expr) *core.Expr {
	if x == target {
		return with
	}
	if x == nil {
		return nil
	}
	c := *x
	c.Args = make([]*core.Expr, len(x.Args))
	for i, a := range x.Args {
		c.Args[i] = replaceExpr(a, target, with)
	}
	return &c
}

// pickReduced: API 部分(来源之后)的"每组取一条": 有多少个合法的请求,在这一步真的淘汰了候选(组里 ≥2 行)。
func (e *diffEnv) pickReduced(reqs []core.Params) (int, bool) {
	steps := e.api.Pipeline.Steps
	at := -1
	for i, st := range steps {
		if st.Kind == "pick_per_key" && !strings.HasPrefix(st.From, "来源 ") {
			at = i
		}
	}
	if at < 0 {
		return 0, false
	}
	count := func(upto int, ps core.Params) int {
		evalParams := core.Params{}
		for k, v := range ps {
			evalParams[k] = v
		}
		res, err := core.Eval(&core.Pipeline{Steps: steps[:upto], Params: e.api.Pipeline.Params}, map[string][]core.Row{e.table: e.rowsFor(ps)}, evalParams)
		if err != nil {
			return -1
		}
		return len(res.Rows)
	}
	n := 0
	for _, ps := range reqs {
		if bad, err := e.invalid(e.request(ps)); err != nil || bad {
			continue
		}
		if before, after := count(at, ps), count(at+1, ps); before > after && after >= 0 {
			n++
		}
	}
	return n, true
}

// evalPerDB: 不依赖请求的求值(来源部分): 分库时每个库分别算,结果合在一起(版本的选择在各个库里分别进行)。
func (e *diffEnv) evalPerDB(p *core.Pipeline) (*core.Result, error) {
	if e.api.Shard == nil {
		return core.Eval(p, map[string][]core.Row{e.table: e.rows}, core.Params{})
	}
	parts := map[*pgx.Conn][]core.Row{}
	var order []*pgx.Conn
	for i, r := range e.rows {
		c := e.rowDB[i]
		if _, ok := parts[c]; !ok {
			order = append(order, c)
		}
		parts[c] = append(parts[c], r)
	}
	out := &core.Result{}
	for _, c := range order {
		res, err := core.Eval(p, map[string][]core.Row{e.table: parts[c]}, core.Params{})
		if err != nil {
			return nil, err
		}
		out.Columns = res.Columns
		out.Rows = append(out.Rows, res.Rows...)
	}
	return out, nil
}

// widest: 最宽的请求(可选的参数不传,必填的参数取限制最少的值): 小于 → 最大值,大于 → 最小值,
// 等于 → 出现最多的值,属于 → 全部的值。只要数据里有多行的结果,这个请求就能找到(分页的出口用例要 ≥5 行)。
func (e *diffEnv) widest(params []core.ParamDecl) core.Params {
	ps := core.Params{}
	for _, d := range params {
		if !d.Required {
			continue
		}
		f := e.filter[d.Name]
		count := map[string]int{}
		val := map[string]any{}
		var keys []string
		for _, r := range e.anchors {
			v := r[f.Column]
			if v == nil {
				continue
			}
			k := core.Canonical(v)
			if count[k] == 0 {
				keys = append(keys, k)
				val[k] = v
			}
			count[k]++
		}
		if len(keys) == 0 {
			ps[d.Name] = e.missingFor(f.Column, d.Type.Kind)
			continue
		}
		sort.Slice(keys, func(i, j int) bool { return compareAny(val[keys[i]], val[keys[j]]) < 0 })
		switch f.Op {
		case "lt", "lte":
			ps[d.Name] = val[keys[len(keys)-1]]
		case "gt", "gte":
			ps[d.Name] = val[keys[0]]
		case "in":
			var all []any
			for _, k := range keys {
				all = append(all, val[k])
			}
			ps[d.Name] = all
		default:
			best := keys[0]
			for _, k := range keys {
				if count[k] > count[best] {
					best = k
				}
			}
			ps[d.Name] = val[best]
		}
		if d.List {
			if _, isList := ps[d.Name].([]any); !isList {
				ps[d.Name] = []any{ps[d.Name]}
			}
		}
	}
	return ps
}

// compareAny: 同种类的值比较大小(排序用;不同种类按规范文本)。
func compareAny(a, b any) int {
	switch x := a.(type) {
	case int64:
		if y, ok := b.(int64); ok {
			return cmpOrdered(x, y)
		}
	case *big.Rat:
		if y, ok := b.(*big.Rat); ok {
			return x.Cmp(y)
		}
	case time.Time:
		if y, ok := b.(time.Time); ok {
			return x.Compare(y)
		}
	case string:
		if y, ok := b.(string); ok {
			return strings.Compare(x, y)
		}
	}
	return strings.Compare(core.Canonical(a), core.Canonical(b))
}

func cmpOrdered[T int64 | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// rowsBeforeLimit: 截取(limit)之前的行数(前N条 的截断有没有发生 / 分页有没有多页)。
func (e *diffEnv) rowsBeforeLimit(ps core.Params) (int, error) {
	steps := e.api.Pipeline.Steps
	for len(steps) > 0 && steps[len(steps)-1].Kind == "limit" {
		steps = steps[:len(steps)-1]
	}
	params := core.Params{}
	for k, v := range ps {
		params[k] = v
	}
	if e.exit.DiffParams != nil {
		for k, v := range e.exit.DiffParams(e.api) {
			params[k] = v
		}
	}
	res, err := core.Eval(&core.Pipeline{Steps: steps, Params: e.api.Pipeline.Params}, map[string][]core.Row{e.table: e.rowsFor(ps)}, params)
	if err != nil {
		return 0, err
	}
	return len(res.Rows), nil
}
