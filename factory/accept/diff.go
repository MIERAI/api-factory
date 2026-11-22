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
	nkSeen    []map[string]any               // 已经生成的行的自然键(JSON 的键 → 值;自然键空间很大时沿用,造出版本)
	groupCols map[string]bool                // API 部分"每组取一条"的分组列: 值域缩小到 2 个,组里才有多个候选
	aggKeys   map[string]bool                // 分组(group)的键: 一半的行取池里的值(组的大小有分布)
	tuples    []tupleGroup                   // 组的列表(组属于): 每个字段是一个列表参数,请求 / 求值前按下标对齐(align)
	keySets   [][]string                     // 唯一的列的组合(主键 / 唯一约束 / 多版本来源的自然键);nil = 还没查
	ties      [][2]string                    // 条件里互相比较的两列(固定条件: 税込 = 旧税込): 数据里要经常相等
	others    []*otherTable                  // 关联的表(多表: 被引用的在前,按生成的顺序;difflookup.go)
	fks       map[string][]fkRef             // 表 → 它的外键(关联条件推出来的: 这些列的值要从对方的键里取)
	view      core.Schema                    // 来源 + 关联之后的列(筛选的列可能是关联加的列)
	children  []*childTable                  // 嵌套 / 子聚合的子表(在来源的表之后造;diffnest.go)
	tm        timeState                      // 派生参数(时间运算): 探针、边界(diff_time.go)
}

// rawCol: 投影成的列 → 物理列(没有投影 → 原样)。
func rawCol(rawOf map[string]string, c string) string {
	if r, ok := rawOf[c]; ok {
		return r
	}
	return c
}

// tupleGroup: 一个组的列表输入。members[i] = 第 i 个字段的参数名(ir.MemberParam),cols[i] = 它的列。
type tupleGroup struct {
	field   string
	members []string
	cols    []string
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
	e.aggKeys = map[string]bool{}
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
		// 分组键: 一半的行值域小(大的组),一半的行从一个中等大小的池里取(1 行 / 2 行 / 3 行 … 的组: 件数并列、名次跳号)
		if st.Kind == "group" {
			for _, k := range st.Keys {
				e.groupCols[rawCol(rawOf, k)] = true
				e.aggKeys[rawCol(rawOf, k)] = true
			}
		}
		// 条件里的 cmp(列, 常量) → 这一列的数据要能碰到这个常量(来源的删除值 / 固定条件的常量);
		// cmp(列, 列) → 两列要经常相等(否则 "=" 和 "<>" 的分支总是一样)
		if st.Kind == "filter" {
			var find func(x *core.Expr)
			name := func(c *core.Expr) string {
				if raw, ok := rawOf[c.Name]; ok && strings.HasPrefix(st.From, "来源 ") {
					return raw
				}
				return c.Name
			}
			find = func(x *core.Expr) {
				if x == nil {
					return
				}
				if x.Op == "cmp" && len(x.Args) == 2 {
					a, b := x.Args[0], x.Args[1]
					if a.Op == "const" && b.Op == "col" {
						a, b = b, a
					}
					switch {
					case a.Op == "col" && b.Op == "const" && b.Value != nil:
						e.constFor[name(a)] = append(e.constFor[name(a)], b.Value)
					case a.Op == "col" && b.Op == "col":
						e.ties = append(e.ties, [2]string{name(a), name(b)})
					}
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
		if rf := e.api.Request.Field(f.Field); f.Op == "in_tuples" && rf != nil && rf.Message != nil { // 组的字段各自的约束
			for i := range rf.Message.Fields {
				if d := constraints.DomainOf(&rf.Message.Fields[i]); d != nil {
					e.domains[rf.Message.Fields[i].Name] = d
				}
			}
			continue
		}
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
	b, err := json.Marshal(withoutNow(req))
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
		if f.Op == "in_tuples" { // 组的列表: 每个字段当作一个"属于"的列表参数生成,用的时候再对齐成组
			g := tupleGroup{field: f.Field, cols: f.Columns}
			for _, c := range f.Columns {
				m := ir.MemberParam(f.Field, c)
				g.members = append(g.members, m)
				env.filter[m] = ir.Filter{Field: m, Column: c, Op: "in", Presence: f.Presence}
			}
			env.tuples = append(env.tuples, g)
			continue
		}
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
	if err := env.initOthers(); err != nil {
		return fail("关联: %v", err)
	}
	if env.out, err = core.Check(p); err != nil {
		return fail("流水线: %v", err)
	}
	env.collectDerived()
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

	// 没有输入的 API: 只有一种请求,能变的只有数据 → 跑好几份数据,覆盖条件累计起来判定
	// (例: 前N条 取最新的 1 行,"最新的那一行被删除"在一份数据里很难碰到)
	if len(env.userParams()) == 0 && len(env.tm.probes) == 0 {
		var results []Result
		var total coverage
		for k := 0; k < 6; k++ {
			if k > 0 {
				env.rnd = rand.New(rand.NewSource(seed0 + int64(100+k)*7919))
				env.rows, env.anchors = nil, nil
				if inserted, rejected, err = env.load(); err != nil {
					return fail("数据: %v", err)
				}
				if err := env.loadAnchors(); err != nil {
					continue
				}
				reqs = env.requests()
			}
			if len(reqs) > 0 && k%3 != 2 { // 定向造数据: 2/3 的数据让结果里的那一行的最新版本是删除 / 逻辑删除
				if err := env.injectRemoval(reqs[0], k%3); err != nil {
					return fail("数据(删除的版本): %v", err)
				}
			}
			rs, cov := env.measure(url, reqs, inserted, rejected, fmt.Sprintf("数据 %d ", k+1))
			results = append(results, rs...)
			total.add(cov)
		}
		ok, text := total.verdict(env)
		text += " / 没有输入: 6 份数据累计"
		return append(results, Result{Spec: api.ID, Suite: "diff", Case: "覆盖条件", Passed: ok, Detail: text})
	}
	results, cov := env.measure(url, reqs, inserted, rejected, "")
	ok, text := cov.verdict(env)
	if attempts > 1 {
		text += fmt.Sprintf(" / 数据生成 %d 次(覆盖条件不满足时换种子、数据加倍重来;最后 %d 倍)", attempts, env.scale)
	}
	return append(results, Result{Spec: api.ID, Suite: "diff", Case: "覆盖条件", Passed: ok, Detail: text})
}

// injectRemoval: 定向造数据(多版本的来源)。没有输入的 API 只有一种请求,删除 / 逻辑删除要看得出来,
// 结果里的那一行的键就要有"最新的版本是删除"。在当前结果的第一行的键上追加一个更新的版本:
// mode 0 = 操作列 = 删除值;mode 1 = 逻辑删除的键 = 逻辑删除值(没有逻辑删除时用 mode 0)。
// 不处理删除的实现会把这一行返回出来 → 结果一定不同。
func (e *diffEnv) injectRemoval(req core.Params, mode int) error {
	st := e.plan.Storage(e.api.Table)
	if st == nil || len(st.NaturalKey) == 0 {
		return nil
	}
	req = e.align(req)
	res, err := core.Eval(e.api.Pipeline, e.evalTables(req), req)
	if err != nil || len(res.Rows) == 0 {
		return err
	}
	top := res.Rows[0]
	// 结果的一行 → 来源的逻辑行(输出的列全部相等;改名的输出按来源的列)
	var anchor core.Row
	for _, a := range e.anchors {
		match, n := true, 0
		for _, c := range res.Columns {
			col := c.Name
			if src, ok := e.api.Rename[col]; ok {
				col = src
			}
			if v, ok := a[col]; ok {
				n++
				if core.Canonical(v) != core.Canonical(top[c.Name]) {
					match = false
					break
				}
			}
		}
		if match && n > 0 {
			anchor = a
			break
		}
	}
	if anchor == nil {
		return nil
	}
	keyOf := map[string]string{} // 逻辑列 → JSON 的键
	for _, f := range st.Fields {
		keyOf[f.Name] = f.Key
	}
	// 这个键最新的物理行(按登记顺序)当模板
	best := -1
	for i, r := range e.rows {
		doc, _ := r[st.DataColumn].(map[string]any)
		same := doc != nil
		for _, k := range st.NaturalKey {
			if !same {
				break
			}
			same = core.Canonical(jsonScalar(doc[keyOf[k]])) == core.Canonical(jsonScalar(anchor[k]))
		}
		if same && (best < 0 || newer(r, e.rows[best], st.OrderBy)) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	row := core.Row{}
	for k, v := range e.rows[best] {
		row[k] = v
	}
	doc := map[string]any{}
	for k, v := range e.rows[best][st.DataColumn].(map[string]any) {
		doc[k] = v
	}
	if mode == 1 && st.LogicalDeleteKey != "" {
		doc[st.LogicalDeleteKey] = st.LogicalDeleteValue
	} else {
		row[st.ActionColumn] = st.DeleteValue
	}
	b, _ := json.Marshal(doc)
	row[st.DataColumn] = string(b)
	for _, c := range st.OrderBy { // 比所有的行都新
		var mx any
		for _, r := range e.rows {
			if mx == nil || core.Compare(r[c], mx) > 0 {
				mx = r[c]
			}
		}
		switch x := mx.(type) {
		case int64:
			row[c] = x + 1
		case time.Time:
			row[c] = x.Add(time.Second)
		}
	}
	var names, holders []string
	var args []any
	for i, c := range e.cols {
		names = append(names, pgx.Identifier{c.Name}.Sanitize())
		holders = append(holders, fmt.Sprintf("$%d::%s", i+1, c.Type.Kind.SQLType()))
		args = append(args, sqlValue(row[c.Name]))
	}
	conn := e.rowDB[best]
	if _, err := conn.Exec(e.ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", pgx.Identifier{e.table}.Sanitize(),
		strings.Join(names, ", "), strings.Join(holders, ", ")), args...); err != nil {
		return err
	}
	row[st.DataColumn] = doc
	e.rows = append(e.rows, row)
	e.rowDB = append(e.rowDB, conn)
	return e.loadAnchors()
}

// jsonScalar: JSON 里的数字(json.Number)→ 和逻辑行一样的值(比较用)。
func jsonScalar(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		return n.String()
	}
	return v
}

func newer(a, b core.Row, order []string) bool {
	for _, c := range order {
		if d := core.Compare(a[c], b[c]); d != 0 {
			return d > 0
		}
	}
	return false
}

// coverage: 一份(或累计的几份)数据上的覆盖。
type coverage struct {
	inserted, rejected, anchors int
	hits                        map[string]int // 0 行 / 1 行 / 多行 / 不合法 / 出口的标签
	hasVersions                 bool
	versions, removed           int
	hasPick                     bool
	pick                        int
	hasRows                     bool
	rowsMin, rowsNeed, rowsGot  int
	sensDesc                    []string
	sensN                       []int
	distinct                    int
}

func (c *coverage) add(o coverage) {
	c.inserted, c.rejected, c.anchors = c.inserted+o.inserted, c.rejected+o.rejected, c.anchors+o.anchors
	if c.hits == nil {
		c.hits = map[string]int{}
	}
	for k, v := range o.hits {
		c.hits[k] += v
	}
	c.hasVersions, c.versions, c.removed = c.hasVersions || o.hasVersions, c.versions+o.versions, c.removed+o.removed
	c.hasPick, c.pick = c.hasPick || o.hasPick, c.pick+o.pick
	c.hasRows, c.rowsGot = c.hasRows || o.hasRows, c.rowsGot+o.rowsGot
	if o.hasRows {
		c.rowsMin, c.rowsNeed = o.rowsMin, o.rowsNeed
	}
	if c.sensDesc == nil {
		c.sensDesc, c.sensN = o.sensDesc, make([]int, len(o.sensN))
	}
	for i := range o.sensN {
		if i < len(c.sensN) {
			c.sensN[i] += o.sensN[i]
		}
	}
	c.distinct += o.distinct
}

// measure: 在当前的数据上跑请求和出口的用例,并统计覆盖。prefix: 用例名的前缀(几份数据时区分)。
func (env *diffEnv) measure(url string, reqs []core.Params, inserted, rejected int, prefix string) ([]Result, coverage) {
	api, ex := env.api, env.exit
	var results []Result
	hits := map[string]int{}
	for i, params := range reqs {
		name := fmt.Sprintf("%s#%02d %s", prefix, i+1, env.describe(params))
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
				results = append(results, Result{Spec: api.ID, Suite: "diff", Case: prefix + c.Name, Passed: false, Detail: c.Fail})
				continue
			}
			ok, detail := runCase(url, c.Case, &api.Response)
			results = append(results, Result{Spec: api.ID, Suite: "diff", Case: prefix + c.Name, Passed: ok, Detail: detail})
		}
	}
	cov := coverage{inserted: inserted, rejected: rejected, anchors: len(env.anchors), hits: hits}
	cov.versions, cov.removed, cov.hasVersions = env.versionCounts()
	cov.pick, cov.hasPick = env.pickReduced(reqs)
	if ex.DiffRowsNeed != nil {
		cov.hasRows = true
		cov.rowsMin, cov.rowsNeed = ex.DiffRowsNeed(api)
		for _, ps := range reqs {
			if bad, err := env.invalid(env.request(ps)); err != nil || bad {
				continue
			}
			if n, err := env.rowsBeforeLimit(ps); err == nil && n >= cov.rowsMin {
				cov.rowsGot++
			}
		}
	}
	cov.sensDesc, cov.sensN, cov.distinct = env.sensCounts(reqs)
	return results, cov
}

// verdict: 覆盖条件。找到 / 找不到都出现;出口要求多行时(分页 / 多条)还要出现多行 …
func (c coverage) verdict(env *diffEnv) (bool, string) {
	hits := c.hits
	text := fmt.Sprintf("数据 %d 行(拒绝 %d,来源 %d 行)/ 结果 0 行 %d 次、1 行 %d 次、多行 %d 次、不合法 %d 次", c.inserted, c.rejected, c.anchors, hits["0"], hits["1"], hits["n"], hits["invalid"])
	ok := hits["0"] > 0 && hits["1"]+hits["n"] > 0
	if len(env.userParams()) == 0 { // 没有输入: "0 行"不要求
		ok = hits["1"]+hits["n"] > 0
	}
	if env.exit.DiffMulti && !env.atMostOneRow() {
		ok = ok && hits["n"] > 0
	}
	if env.exit.DiffCover != nil {
		text += env.exit.DiffCover(env.api, hits)
	}
	// 多版本的来源(每键取一条): 多版本的键 / 被删除的键 都要 ≥ 3,否则版本的机制没有被测到
	if c.hasVersions {
		text += fmt.Sprintf(" / 来源: 多出来的旧版本 %d 行、最新是删除或逻辑删除的键 %d 个", c.versions, c.removed)
		ok = ok && c.versions >= 3 && c.removed >= 3
	}
	// 每组取一条(API 部分): 组里有 ≥2 个候选、真的淘汰掉了行的请求要 ≥ 3
	if c.hasPick {
		text += fmt.Sprintf(" / 每组取一条淘汰了候选的请求 %d 个", c.pick)
		ok = ok && c.pick >= 3
	}
	if c.hasRows { // 出口要的数据(分页: ≥5 行;前N条: 超过 N 行的请求 ≥3)
		text += fmt.Sprintf(" / 结果 ≥%d 行的请求 %d 个(出口要 %d 个)", c.rowsMin, c.rowsGot, c.rowsNeed)
		ok = ok && c.rowsGot >= c.rowsNeed
	}
	if len(c.sensDesc) > 0 {
		need := min(3, c.distinct)
		var weak []string
		for i, d := range c.sensDesc {
			if c.sensN[i] < need {
				weak = append(weak, fmt.Sprintf("%s(%d 个请求)", d, c.sensN[i]))
			}
		}
		text += fmt.Sprintf(" / 起作用的条件 %d/%d", len(c.sensDesc)-len(weak), len(c.sensDesc))
		if len(weak) > 0 {
			text += fmt.Sprintf("(不足 3 个请求: %s)", strings.Join(weak, "、"))
			ok = false
		}
	}
	return ok, text
}

// ---------------------------------------------------------------------------
// 数据
// ---------------------------------------------------------------------------

// atMostOneRow: 每个合法的请求最多返回 1 行(取 1 条;或者多版本来源的自然键全部被必填的"等于"固定)。
// 这时"多行的结果"不可能出现,出口的这个覆盖条件不要求。(普通的表的主键要查约束,还没有处理。)
func (e *diffEnv) atMostOneRow() bool {
	for _, st := range e.api.Pipeline.Steps { // 前N条 的 条数 1
		if st.Kind == "limit" && st.Param == "" && st.N == 1 {
			return true
		}
	}
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
// 复合的 → 最后一个不是分组列(groupCols: 请求固定的必填等于 / 属于的列、分库键)的列,前面的列保持小值域,形成分组
// (例如 (order_id, line_no) 每个订单有多行)。分组列取唯一值时,请求固定它就最多 1 行,多行的结果永远不出现
// (例: 主键 (sales_date, store_code) + 必填的 store_code 等于 → sales_date 取唯一值,一个店有多天)。
// 所有列都是分组列 → 最后一列(和以前一样)。
func (e *diffEnv) uniqueColumns() (map[string]bool, error) {
	rows, err := e.conn.Query(e.ctx, `
		SELECT tc.constraint_name, kcu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
		WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
		ORDER BY tc.constraint_name, kcu.ordinal_position`, e.table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	cols := map[string][]string{}
	for rows.Next() {
		var name, col string
		if err := rows.Scan(&name, &col); err != nil {
			return nil, err
		}
		if _, ok := cols[name]; !ok {
			names = append(names, name)
		}
		cols[name] = append(cols[name], col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	ties := e.tieCols()
	for _, name := range names {
		out[uniqueColumnOf(cols[name], e.groupCols, ties)] = true
	}
	return out, nil
}

// tieCols: 排序里需要平局的列 = 每个排序的最后一列以外的列(数据源的列名)。这些列取唯一值的话,后面的排序列
// 永远比不到,"排序的第 N 列的方向"的覆盖条件不可能满足(例: 排序 [proc_date 新到旧, store_code 大到小],
// proc_date 唯一 → 同一天只有一行)。
func (e *diffEnv) tieCols() map[string]bool {
	out := map[string]bool{}
	if e.api.Pipeline == nil {
		return out
	}
	for _, st := range e.api.Pipeline.Steps {
		if st.Kind != "sort" {
			continue
		}
		for j := 0; j < len(st.Order)-1; j++ {
			out[e.srcCol(st.Order[j].Col)] = true
		}
	}
	return out
}

// uniqueColumnOf: 约束的列(按顺序)里取唯一值的那一列,从后往前找:
// 既不是分组列、也不需要平局的列 → 不是分组列的列 → 最后一列。
func uniqueColumnOf(cols []string, group, ties map[string]bool) string {
	for i := len(cols) - 1; i >= 0; i-- {
		if !group[cols[i]] && !ties[cols[i]] {
			return cols[i]
		}
	}
	for i := len(cols) - 1; i >= 0; i-- {
		if !group[cols[i]] {
			return cols[i]
		}
	}
	return cols[len(cols)-1]
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
		for _, t := range e.ties { // 互相比较的两列: 约 1/3 的行相等
			ka, kb := e.jsonKeyOf(c.Name, t[0]), e.jsonKeyOf(c.Name, t[1])
			if ka != "" && kb != "" && e.rnd.Intn(3) == 0 {
				if v, ok := obj[ka]; ok {
					obj[kb] = v
				}
			}
		}
		// 自然键的组合空间很大(自然键的列多,随机的值几乎不会重复)→ 版本链只有一条,版本的机制测不到。
		// 约 1/3 的行沿用之前某一行的整组自然键(同一个键的新版本)
		if e.nkSpace >= diffRows*3/5*max(e.scale, 1) && len(e.nkSeen) > 0 && e.rnd.Intn(3) == 0 {
			prev := e.nkSeen[e.rnd.Intn(len(e.nkSeen))]
			for k, v := range prev {
				obj[k] = v
			}
		} else if len(e.nkCols) > 0 {
			key := map[string]any{}
			for _, k := range e.json[c.Name] {
				if e.nkCols[e.keyCol[k.key]] {
					key[k.key] = obj[k.key]
				}
			}
			e.nkSeen = append(e.nkSeen, key)
		}
		b, _ := json.Marshal(obj)
		return string(b)
	}
	return e.scalarFor(c.Name, c.Type.Kind, unique, i)
}

// jsonKeyOf: JSON 列里,投影成逻辑列 col 的键(没有 → "")。
func (e *diffEnv) jsonKeyOf(jsonCol, col string) string {
	for _, k := range e.json[jsonCol] {
		if e.keyCol[k.key] == col {
			return k.key
		}
	}
	return ""
}

// scalarFor: 某一列的值(这一列有范围值域时从里面取)。
func (e *diffEnv) scalarFor(col string, kind core.Kind, unique bool, i int) any {
	if e.aggKeys[col] && !e.nkCols[col] && !unique && e.rnd.Intn(2) == 0 { // 分组键: 池的大小 = 8 × 数据的倍数
		if v := e.poolValue(col, kind, e.rnd.Intn(8*max(e.scale, 1))); v != nil {
			return v
		}
	}
	// 和派生参数比较的列: 约 2/3 的行落在某个探针的边界上(−1 / 正好 / +1)。唯一列也这样(重复的行被数据库拒绝、跳过)
	if len(e.tm.cmps) > 0 && (kind == core.Date || kind == core.Timestamp) && e.rnd.Intn(3) != 0 {
		if v := e.boundaryValue(col); v != nil {
			return v
		}
	}
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

// poolValue: 分组键的池里第 x 个值(和值域里的 3 个密集的值不同)。有约束 / 枚举的列: 约束里扩大,扩大不了 → nil。
func (e *diffEnv) poolValue(col string, kind core.Kind, x int) any {
	if d, ok := e.domains[col]; ok {
		if d.Expand != nil {
			return d.Expand(2000 + x)
		}
		return nil
	}
	if e.plan.Enum(e.table, col) != nil {
		return nil
	}
	switch kind {
	case core.Int32, core.Int64:
		return int64(100 + x)
	case core.Text:
		return fmt.Sprintf("g%02d", x)
	case core.Decimal:
		return big.NewRat(int64(100+x), 4)
	case core.Date, core.Timestamp:
		return e.scalar(kind, true, 10+x)
	}
	return nil
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
	if unique {
		switch t.Domain[0].(type) {
		case time.Time: // 日期 / 时间的唯一列: 值域中间的时刻 ±0, -1, +1, -2, +2 … 天(以前从 3 个值里随机取 → 主键冲突,行被拒绝;
			// 围绕请求用的那几个值分布 → 大于 / 小于的筛选切掉约一半)
			center := t.Domain[len(t.Domain)/2].(time.Time)
			off := (i + 1) / 2
			if i%2 == 1 {
				off = -off
			}
			return center.AddDate(0, 0, off)
		case *big.Rat: // 十进制的唯一列: 序号
			return big.NewRat(int64(i+1), 1)
		}
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
	e.nkSeen = nil // 每次生成数据都重新开始
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
	if err := e.loadOthers(); err != nil { // 关联的表先造(外键的值从它们的键里取)
		return 0, 0, err
	}
	// 分库键在物理行里的值: 普通的表 = 列;其他来源由来源决定(JSON版本 = JSON 里的键)
	st := e.plan.Storage(e.api.Table)
	var src *sources.Source
	if st != nil {
		src, _ = sources.ByKind(st.Kind)
	}
	gen, err := e.generatedCols(e.table) // 生成列不插入(值由数据库算)
	if err != nil {
		return 0, 0, err
	}
	var names, holders []string
	for _, c := range e.cols {
		if gen[c.Name] {
			continue
		}
		names = append(names, pgx.Identifier{c.Name}.Sanitize())
		holders = append(holders, fmt.Sprintf("$%d::%s", len(holders)+1, c.Type.Kind.SQLType()))
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
			physical[c.Name] = e.value(c, i, unique[c.Name])
		}
		for _, t := range e.ties { // 普通的表: 互相比较的两列约 1/3 的行相等
			_, okA := physical[t[0]]
			_, okB := physical[t[1]]
			if okA && okB && !unique[t[1]] && e.rnd.Intn(3) == 0 {
				physical[t[1]] = physical[t[0]]
			}
		}
		e.applyFKs(e.table, e.cols, physical, unique, i)
		for _, c := range e.cols {
			if !gen[c.Name] {
				args = append(args, sqlValue(physical[c.Name]))
			}
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
	if err := e.loadChildren(); err != nil { // 嵌套 / 子聚合的子表: 父行(来源的表)读回之后造
		return 0, 0, err
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
	out = append(out, e.directed(out, seen)...)
	return e.spreadProbes(out) // 派生参数: 每个请求带一组探针(now + 输入)
}

func (e *diffEnv) describe(ps core.Params) string {
	ps = e.align(ps)
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
		col := column
		if src, ok := e.api.Rename[column]; ok { // 输出改名: 枚举的映射按来源的列
			col = src
		}
		if en := e.plan.Enum(e.api.Table, col); en != nil {
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
	if t, ok := e.view.Col(name); ok {
		return t, true
	}
	return core.Schema(e.cols).Col(name)
}

func (e *diffEnv) request(ps core.Params) map[string]any {
	ps = e.align(ps)
	req := map[string]any{}
	member := map[string]bool{}
	for _, g := range e.tuples {
		l, ok := ps[g.members[0]].([]any)
		if !ok {
			continue
		}
		items := make([]any, len(l))
		for k := range l {
			item := map[string]any{}
			for i, m := range g.members {
				item[g.cols[i]] = e.present(g.cols[i], ps[m].([]any)[k])
			}
			items[k] = item
		}
		req[g.field] = items
		for _, m := range g.members {
			member[m] = true
		}
	}
	for k, v := range ps {
		if member[k] {
			continue
		}
		if k == core.NowParam { // now: 请求头 X-Factory-Now(不是请求字段)
			req[NowKey] = nowText(v)
			continue
		}
		if x, ok := e.presentInput(k, v); ok { // 派生参数的输入: 按参数的类型表示(没有对应的列)
			req[k] = x
			continue
		}
		req[k] = e.present(e.filter[k].Column, v)
	}
	return req
}

// align: 组的列表的各个字段对齐成同样长的列表(确定性的)。第 k 个组缺的字段从数据里找:
// 和已知的字段都相等的第一行(现存的行优先,再找旧版本);找不到时用数据里不存在的值。
// 随机 / 扫描 / 定向生成的请求只设了部分字段时也能变成合法的组。
func (e *diffEnv) align(ps core.Params) core.Params {
	ps = e.complete(ps) // 派生参数: 缺少探针的请求补上一组(确定的)
	if len(e.tuples) == 0 {
		return ps
	}
	var out core.Params
	for _, g := range e.tuples {
		lists := make([][]any, len(g.members))
		n, present, aligned := 0, false, true
		for i, m := range g.members {
			l, ok := ps[m].([]any)
			present = present || ok
			lists[i] = l
			n = max(n, len(l))
		}
		for _, l := range lists {
			aligned = aligned && len(l) == n
		}
		if !present || aligned {
			continue
		}
		if out == nil {
			out = core.Params{}
			for k, v := range ps {
				out[k] = v
			}
		}
		full := make([][]any, len(g.members))
		cands := [][]core.Row{e.anchors, e.staleRows()}
		for k := 0; k < n; k++ {
			var row core.Row
			for _, cand := range cands {
				for _, r := range cand {
					match := true
					for i, l := range lists {
						if k < len(l) && (r[g.cols[i]] == nil || core.Compare(r[g.cols[i]], l[k]) != 0) {
							match = false
							break
						}
					}
					if match {
						row = r
						break
					}
				}
				if row != nil {
					break
				}
			}
			for i, l := range lists {
				var v any
				switch {
				case k < len(l):
					v = l[k]
				case row != nil && row[g.cols[i]] != nil:
					v = row[g.cols[i]]
				default:
					t, _ := e.colType(g.cols[i])
					v = e.missingFor(g.cols[i], t.Kind)
				}
				full[i] = append(full[i], v)
			}
		}
		for i, m := range g.members {
			out[m] = full[i]
		}
	}
	if out == nil {
		return ps
	}
	return out
}

func (e *diffEnv) item(r core.Row, msg *ir.Message) map[string]any {
	out := map[string]any{}
	for _, f := range msg.Fields {
		if c, ok := r[f.Name].(core.Children); ok && f.Message != nil { // 嵌套的子行(diffnest.go)
			t, _ := e.colType(f.Name)
			out[f.Name] = childItems(c, t, f.Message)
			continue
		}
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
	ps = e.align(ps)
	evalParams := core.Params{}
	for k, v := range ps {
		evalParams[k] = v
	}
	if e.exit.DiffParams != nil {
		for k, v := range e.exit.DiffParams(e.api) {
			evalParams[k] = v
		}
	}
	res, err := core.Eval(e.api.Pipeline, e.evalTables(ps), evalParams)
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
		// 关联也算来源: 筛选的列可能是关联加的列,请求的值要从关联之后的行里取
		if st.From != "datasource.table" && !strings.HasPrefix(st.From, "来源 ") && st.Kind != "lookup" {
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
	// mutate: 对流水线的另一种改动(每组取一条的顺序反过来 / 可以为空的键去掉空值 …)。nil = 把 node 换成恒真
	mutate func(p *core.Pipeline) *core.Pipeline
	// falseNode: 把 node 换成恒假(「或者」的一个分支)
	falseNode bool
	// custom: 错误的实现的结果(规范文本),不能写成流水线的改动时(分组 / 名次: diff_group.go)
	custom func(ps core.Params) string
	// cond: 不是错误的实现,而是请求要满足的条件(分组: 有多行的组 …);满足的请求数 = 起作用的请求数
	cond func(ps core.Params) bool
}

// onlyKeysOut: 第 i 步(每组取一条)之后的投影只输出分组键。
func onlyKeysOut(p *core.Pipeline, i int) bool {
	keys := map[string]bool{}
	for _, k := range p.Steps[i].Keys {
		keys[k] = true
	}
	for _, st := range p.Steps[i+1:] {
		if st.Kind != "project" {
			continue
		}
		for _, x := range st.Project {
			if x.Expr.Op != "col" || !keys[x.Expr.Name] {
				return false
			}
		}
		next := map[string]bool{}
		for _, x := range st.Project {
			next[x.Name] = true
		}
		keys = next
	}
	return true
}

// mutated: 这个对象对应的"错误的实现"(覆盖条件要求 ≥3 个请求能把它和正确的结果区分开)。
func (t sensTarget) mutated(p *core.Pipeline) *core.Pipeline {
	if t.mutate != nil {
		return t.mutate(p)
	}
	mut := *p
	mut.Steps = append([]core.Step(nil), p.Steps...)
	mut.Steps[t.step].Pred = replaceExpr(p.Steps[t.step].Pred, t.node, core.Const(!t.falseNode, core.Type{Kind: core.Bool}))
	return &mut
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
		targets = append(targets, sensTarget{step: i, node: st.Pred, desc: st.From})
		// API 的固定条件(没有请求参数)可能是 UNKNOWN(列为空)时: 把 UNKNOWN 当成 TRUE(空值的行留下)的实现要看得出来
		if !source && !refersParam(st.Pred) && refersColumn(st.Pred) {
			if schema, err := core.Check(&core.Pipeline{Steps: p.Steps[:i], Params: p.Params}); err == nil {
				if t, err := p.TypeOf(st.Pred, schema); err == nil && t.Nullable {
					i, pred := i, st.Pred
					targets = append(targets, sensTarget{node: pred, desc: st.From + " 的空值(不确定当成成立)", mutate: func(p *core.Pipeline) *core.Pipeline {
						mut := *p
						mut.Steps = append([]core.Step(nil), p.Steps...)
						mut.Steps[i].Pred = core.Or(pred, core.IsNull(pred))
						return &mut
					}})
				}
			}
		}
		var walk func(x *core.Expr)
		walk = func(x *core.Expr) {
			if x == nil {
				return
			}
			if x.Op == "and" {
				for j, a := range x.Args {
					targets = append(targets, sensTarget{step: i, node: a, desc: fmt.Sprintf("%s 的第 %d 项", st.From, j+1)})
				}
			}
			if x.Op == "or" && !refersParam(x) { // 固定条件的「或者」: 每个分支换成 FALSE(漏掉这个分支)也要看得出来
				for j, a := range x.Args {
					targets = append(targets, sensTarget{step: i, node: a, desc: fmt.Sprintf("%s 的「或者」第 %d 项", st.From, j+1), falseNode: true})
				}
			}
			if x.Op == "in_tuples" { // 组属于: 按列各自"属于"(组的交叉组合也算命中)的实现要看得出来
				i, node := i, x
				var each []*core.Expr
				for j, c := range x.Args {
					each = append(each, core.InList(c, x.Params[j]))
				}
				cross := core.And(each...)
				targets = append(targets, sensTarget{step: i, node: x, desc: st.From + " 的组(按列各自属于)", mutate: func(p *core.Pipeline) *core.Pipeline {
					mut := *p
					mut.Steps = append([]core.Step(nil), p.Steps...)
					mut.Steps[i].Pred = replaceExpr(p.Steps[i].Pred, node, cross)
					return &mut
				}})
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
	// 排序的次级列: 只把这一列的方向反过来也要看得出来(前面的列相同的行之间的顺序)。
	// 要求的列 = 第 2 列 + 方向和前一列不同的列(错误的实现是"方向跟着前一列 / 第一列走";排序是逐列同样生成的,
	// 方向相同的后续列不单独要求 —— 10 列的键要每一列都区分,需要前 9 列都平局的数据)。
	// 前面的列已经唯一(包含主键 / 自然键 / 每组取一条的键)时没有平局,反过来是等价的,不要求。
	for i, st := range p.Steps {
		if st.Kind != "sort" || len(st.Order) < 2 {
			continue
		}
		if e.atMostOneRow() {
			continue
		}
		fixed := e.fixedCols()
		prev := -1 // 前一个不是常量的列
		for j := 0; j < len(st.Order); j++ {
			if fixed[e.srcCol(st.Order[j].Col)] { // 请求固定了这一列(必填的等于): 常量,方向无所谓
				continue
			}
			p0 := prev
			prev = j
			if p0 < 0 { // 第一列(的方向由别的覆盖条件和出口用例测)
				continue
			}
			if e.uniquePrefix(st.Order[:j]) {
				break
			}
			first := true
			for k := 0; k < p0; k++ {
				first = first && fixed[e.srcCol(st.Order[k].Col)]
			}
			if !first && st.Order[j].Desc == st.Order[p0].Desc { // 第一个次级列以外: 方向变化的地方才要求
				continue
			}
			i, j := i, j
			out = append(out, sensTarget{desc: fmt.Sprintf("排序的第 %d 列(%s)的方向", j+1, st.Order[j].Col), mutate: func(p *core.Pipeline) *core.Pipeline {
				mut := *p
				mut.Steps = append([]core.Step(nil), p.Steps...)
				order := append([]core.SortKey(nil), p.Steps[i].Order...)
				order[j].Desc = !order[j].Desc
				mut.Steps[i].Order = order
				return &mut
			}})
		}
	}
	out = append(out, e.groupTargets()...)
	for i, st := range p.Steps {
		if st.Kind != "pick_per_key" {
			continue
		}
		source := strings.HasPrefix(st.From, "来源 ")
		if !source { // 每组取一条: 组内的顺序反过来(取最大 ↔ 取最小)要看得出来 —— 只靠"淘汰了候选"不够,候选的输出可能都一样
			if onlyKeysOut(p, i) { // 只输出分组键(存在检查): 取哪一行输出都一样,顺序反过来在语义上等价,不要求
				continue
			}
			i := i
			out = append(out, sensTarget{desc: st.From + " 的顺序", mutate: func(p *core.Pipeline) *core.Pipeline {
				mut := *p
				mut.Steps = append([]core.Step(nil), p.Steps...)
				order := append([]core.SortKey(nil), p.Steps[i].Order...)
				for j := range order {
					order[j].Desc = !order[j].Desc
				}
				mut.Steps[i].Order = order
				return &mut
			}})
			continue
		}
		// 多版本来源的可以为空的键: 键为空的行要出现在结果里(丢掉它们的实现要看得出来)
		before := &core.Pipeline{Steps: p.Steps[:i], Params: p.Params}
		schema, err := core.Check(before)
		if err != nil {
			continue
		}
		for _, k := range st.Keys {
			if e.narrowedOut(k) || e.fixedCols()[k] { // 必填的等于固定了这一列: 空值的行不会通过 // API 的必填条件拒绝空值(类型收窄后输出里这一列不为空): 空键的组本来就进不了结果,去掉它们是等价的
				continue
			}
			if t, ok := schema.Col(k); ok && t.Nullable {
				i, k := i, k
				out = append(out, sensTarget{desc: "可以为空的键 " + k, mutate: func(p *core.Pipeline) *core.Pipeline {
					mut := *p
					mut.Steps = append(append(append([]core.Step(nil), p.Steps[:i]...), core.Filter(core.Not(core.IsNull(core.Col(k))))), p.Steps[i:]...)
					return &mut
				}})
			}
		}
	}
	return append(append(append(out, e.lookupTargets()...), e.nestTargets()...), e.timeTargets()...) // 关联 / 嵌套 / 派生参数
}

// refersParam: 表达式里有没有请求参数(param / absent / 列表参数的元件)。
func refersParam(x *core.Expr) bool {
	if x == nil {
		return false
	}
	switch x.Op {
	case "param", "absent", "in_list", "starts_with", "tuple_after", "in_tuples", "now":
		return true
	}
	for _, a := range x.Args {
		if refersParam(a) {
			return true
		}
	}
	return false
}

// uniquePrefix: 排序的这几列(输出的名字)能不能唯一地确定一行。
func (e *diffEnv) uniquePrefix(order []core.SortKey) bool {
	have := map[string]bool{}
	for c := range e.fixedCols() { // 请求固定的列在结果里是常量,也算
		have[c] = true
	}
	for _, o := range order {
		have[e.srcCol(o.Col)] = true
	}
	covers := func(set []string) bool {
		for _, c := range set {
			if !have[c] {
				return false
			}
		}
		return len(set) > 0
	}
	if e.keySets == nil {
		e.keySets = [][]string{}
		rows, err := e.conn.Query(e.ctx, `
			SELECT array_agg(kcu.column_name::text ORDER BY kcu.ordinal_position)
			FROM information_schema.table_constraints tc
			JOIN information_schema.key_column_usage kcu
			  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
			WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
			GROUP BY tc.constraint_name`, e.table)
		if err == nil {
			for rows.Next() {
				var set []string
				if rows.Scan(&set) == nil {
					e.keySets = append(e.keySets, set)
				}
			}
			rows.Close()
		}
		for _, st := range e.api.Pipeline.Steps { // 每组取一条(来源的取最新 / API 的每组取一条)之后,分组键唯一
			if st.Kind == "pick_per_key" {
				e.keySets = append(e.keySets, st.Keys)
			}
		}
		if gi := groupIndex(e.api.Pipeline, "group"); gi >= 0 { // 分组之后: 唯一的只有分组键(表的主键已经不在输出里)
			e.keySets = [][]string{e.api.Pipeline.Steps[gi].Keys}
		}
	}
	for _, set := range e.keySets {
		if covers(set) {
			return true
		}
	}
	return false
}

// srcCol: 输出的名字 → 来源的列(输出改名)。
func (e *diffEnv) srcCol(name string) string {
	if src, ok := e.api.Rename[name]; ok {
		return src
	}
	return name
}

// fixedCols: 必填的「等于」(单列、不是列表)固定的列 + 分库键: 一个请求的结果里是常量。
func (e *diffEnv) fixedCols() map[string]bool {
	out := map[string]bool{}
	for _, d := range e.api.Pipeline.Params {
		f, ok := e.filter[d.Name]
		if ok && d.Required && !d.List && f.Op == "eq" && len(f.Columns) == 0 {
			out[f.Column] = true
		}
	}
	if sh := e.api.Shard; sh != nil {
		out[sh.Column] = true
	}
	return out
}

// narrowedOut: 来源的列 col 在最终的输出里(可能改了名)被收窄成不为空。
func (e *diffEnv) narrowedOut(col string) bool {
	name := col
	for out, src := range e.api.Rename {
		if src == col {
			name = out
		}
	}
	t, ok := e.out.Col(name)
	return ok && !t.Nullable
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
			if e.sensitiveTo(ps, t) {
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
				if bad, err := e.invalid(e.request(ps)); err != nil || bad || !e.sensitiveTo(ps, t) {
					continue
				}
				seen[k] = true
				out = append(out, ps)
				valid = append(valid, ps)
				n++
			}
		}
		// 两行一起: 列表参数放两行的值(组属于的交叉组合 / 属于的多值),标量参数取第一行
		if n < 3 && len(rows) > 1 {
			m := min(len(rows), 40)
		pairs:
			for a := 0; a < m; a++ {
				for b := 0; b < m; b++ {
					if n >= 3 {
						break pairs
					}
					if a == b {
						continue
					}
					ps := fromRow(rows[a], 0)
					for k, v := range fromRow(rows[b], 0) {
						if l, ok := v.([]any); ok {
							if cur, ok := ps[k].([]any); ok {
								ps[k] = append(append([]any(nil), cur...), l...)
							}
						}
					}
					key := e.describe(ps)
					if seen[key] {
						continue
					}
					if bad, err := e.invalid(e.request(ps)); err != nil || bad || !e.sensitiveTo(ps, t) {
						continue
					}
					seen[key] = true
					out = append(out, ps)
					valid = append(valid, ps)
					n++
				}
			}
		}
	}
	return out
}

// sensCounts: 每个条件(sensTargets)起作用的请求数,和合法请求的种类数。
func (e *diffEnv) sensCounts(reqs []core.Params) (descs []string, counts []int, distinct int) {
	p := e.api.Pipeline
	targets := e.sensTargets()
	valid := []core.Params{}
	for _, ps := range reqs {
		if bad, err := e.invalid(e.request(ps)); err == nil && !bad {
			valid = append(valid, ps)
		}
	}
	base := map[int]string{}
	seen := map[string]bool{}
	for i, ps := range valid {
		base[i] = e.evalCanon(p, ps)
		b, _ := json.Marshal(ps)
		seen[string(b)] = true
	}
	for _, t := range targets {
		var mut *core.Pipeline
		if t.custom == nil && t.cond == nil {
			mut = t.mutated(p)
		}
		n := 0
		for i, ps := range valid {
			switch {
			case t.cond != nil:
				if t.cond(ps) {
					n++
				}
			case t.custom != nil:
				if t.custom(ps) != base[i] {
					n++
				}
			case e.evalCanon(mut, ps) != base[i]:
				n++
			}
		}
		descs, counts = append(descs, t.desc), append(counts, n)
	}
	return descs, counts, len(seen)
}

// sensitivity: 起作用的请求不足的条件。要求 ≥3 个请求;可能的请求本来就不到 3 种时,要求每一种。
func (e *diffEnv) sensitivity(reqs []core.Params) (weak []string, total int) {
	descs, counts, distinct := e.sensCounts(reqs)
	need := min(3, distinct)
	for i, d := range descs {
		if counts[i] < need {
			weak = append(weak, fmt.Sprintf("%s(%d 个请求)", d, counts[i]))
		}
	}
	return weak, len(descs)
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
	ps = e.align(ps)
	params := core.Params{}
	for k, v := range ps {
		params[k] = v
	}
	if e.exit.DiffParams != nil {
		for k, v := range e.exit.DiffParams(e.api) {
			params[k] = v
		}
	}
	res, err := core.Eval(pl, e.evalTables(ps), params)
	if err != nil {
		return "error"
	}
	var b strings.Builder
	for _, r := range res.Rows {
		for _, c := range res.Columns {
			b.WriteString(core.Canonical(r[c.Name]) + "|")
		}
		if core.Overflow(r) { // 子行超过上限: 出口返回 FAILED_PRECONDITION(上限差一的实现要看得出来)
			b.WriteString("!over")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sensitive: 把第 step 步条件里的 node 换成恒真后,这个请求的结果变不变。
func (e *diffEnv) sensitive(ps core.Params, step int, node *core.Expr) bool {
	return e.sensitiveTo(ps, sensTarget{step: step, node: node})
}

// sensitiveTo: 这个请求能不能把正确的结果和 t 对应的错误实现区分开。
func (e *diffEnv) sensitiveTo(ps core.Params, t sensTarget) bool {
	switch {
	case t.cond != nil:
		return t.cond(ps)
	case t.custom != nil:
		return t.custom(ps) != e.evalCanon(e.api.Pipeline, ps)
	}
	return e.evalCanon(t.mutated(e.api.Pipeline), ps) != e.evalCanon(e.api.Pipeline, ps)
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
		ps = e.align(ps)
		evalParams := core.Params{}
		for k, v := range ps {
			evalParams[k] = v
		}
		res, err := core.Eval(&core.Pipeline{Steps: steps[:upto], Params: e.api.Pipeline.Params}, e.evalTables(ps), evalParams)
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
		return core.Eval(p, e.tablesIn(nil, e.rows), core.Params{})
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
		res, err := core.Eval(p, e.tablesIn(c, parts[c]), core.Params{})
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
	ps = e.align(ps)
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
	res, err := core.Eval(&core.Pipeline{Steps: steps, Params: e.api.Pipeline.Params}, e.evalTables(ps), params)
	if err != nil {
		return 0, err
	}
	return len(res.Rows), nil
}
