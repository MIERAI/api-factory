package accept

// 差分测试的时间运算(派生参数: now() / add / trunc / local_date(今天))。
//
// 派生参数的值由 now 和请求参数算出来(例: updated_at 早于 now − N 分钟;sales_date 在指定日的上个月)。
// 随机的数据几乎不会正好落在派生出来的边界上,"早于"写成"不晚于"、月末没有截到最后一天、在 UTC 而不是领域的时区截日
// 这些错误就测不到。所以:
//   - 探针(probe): 先定下几组"now + 派生参数的输入"(now 在领域时区的日界线前后;日期取月末、闰年的 2 月末;
//     整数取约束的值域),每个请求带其中一组(now 作为请求头 X-Factory-Now 发出,参考求值器用同一个值);
//   - 数据: 和派生参数比较的列,约 2/3 的行取"某个探针的边界值 −1 / 正好 / +1"(时间 ±1 秒,日期 ±1 天);
//   - 覆盖条件: 边界的 < 和 <=(> 和 >=)、截日用的时区(换成 UTC)、now 的来源(换成别的时刻)各自要被 ≥3 个请求区分。

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"apifactory/factory/constraints"
	"apifactory/factory/core"
	"apifactory/factory/kinds"
)

// derivedCmp: 筛选里"列 op 时间运算的表达式"的一处。
type derivedCmp struct {
	table string // 列所在的表(来源的表;嵌套 / 子聚合的固定条件: 子表)
	step  int
	node  *core.Expr // cmp 节点
	col   string
	expr  *core.Expr // 时间运算的一边
	kind  core.Kind  // 列的类型(日期 / 时间;整数编码的日期 = 日期)
	ymd   bool       // 列是整数编码的日期(条件里是 ymd(列))
	asof  bool       // 按日期的关联的开始 / 结束列(参照 = 今天 / 现在): 只用来造边界的值,覆盖条件在 difflookup.go
}

// timeState: 派生参数的状态(没有时间运算的 API 为空,随机数的消耗和以前完全一样)。
type timeState struct {
	cmps    []derivedCmp
	inputs  []core.ParamDecl // 派生参数的输入(请求参数,但不是直接的筛选)
	usesNow bool
	zone    string
	probes  []core.Params   // 每组: NowParam(用到 now 时)+ 输入
	ymdIn   map[string]bool // 整数编码的日期的输入(派生参数里是 ymd(参数))
	ymdCol  map[string]bool // 整数编码的日期的列(和派生参数比较)
	pin     core.Params     // 不为空: 边界值只取这个探针(嵌套的子行按一个探针数通过固定条件的行,diffnest.go)
}

func isTimeExpr(x *core.Expr) bool {
	if x == nil {
		return false
	}
	switch x.Op {
	case "now", "add", "trunc", "local_date":
		return true
	}
	return false
}

// collectDerived: 找出派生参数的比较和输入,定下探针(用自己的随机数: 不影响别的数据)。
func (e *diffEnv) collectDerived() {
	p := e.api.Pipeline
	e.tm = timeState{ymdIn: map[string]bool{}, ymdCol: map[string]bool{}}
	inputs := map[string]bool{}
	var walkIn func(x *core.Expr)
	walkIn = func(x *core.Expr) {
		if x == nil {
			return
		}
		if x.Op == "param" {
			inputs[x.Name] = true
		}
		if x.Op == "ymd" && len(x.Args) == 1 && x.Args[0].Op == "param" {
			e.tm.ymdIn[x.Args[0].Name] = true
		}
		if x.Op == "now" {
			e.tm.usesNow = true
		}
		if (x.Op == "add" || x.Op == "trunc" || x.Op == "local_date") && x.Zone != "" && e.tm.zone == "" {
			e.tm.zone = x.Zone
		}
		for _, a := range x.Args {
			walkIn(a)
		}
	}
	for i, st := range p.Steps {
		// 筛选;嵌套 / 子聚合的固定条件(子表的列和 今天 / 现在 比较: 子行的数据也要落在边界上)
		if (st.Kind != "filter" && st.Kind != "nest" && st.Kind != "child_agg") || st.Pred == nil {
			continue
		}
		table, colType := e.table, e.colType
		if st.Kind != "filter" {
			table = st.Table
			colType = func(n string) (core.Type, bool) { return core.Schema(st.Columns).Col(n) }
		}
		var find func(x *core.Expr)
		find = func(x *core.Expr) {
			if x == nil {
				return
			}
			if x.Op == "cmp" && len(x.Args) == 2 {
				a, b := x.Args[0], x.Args[1]
				if isTimeExpr(a) {
					a, b = b, a
				}
				if a.Op == "col" && isTimeExpr(b) {
					t, _ := colType(a.Name)
					e.tm.cmps = append(e.tm.cmps, derivedCmp{table: table, step: i, node: x, col: a.Name, expr: b, kind: t.Kind})
					walkIn(b)
				}
				if a.Op == "ymd" && a.Args[0].Op == "col" && isTimeExpr(b) { // 整数编码的日期的列
					e.tm.cmps = append(e.tm.cmps, derivedCmp{table: table, step: i, node: x, col: a.Args[0].Name, expr: b, kind: core.Date, ymd: true})
					e.tm.ymdCol[a.Args[0].Name] = true
					walkIn(b)
				}
			}
			for _, a := range x.Args {
				find(a)
			}
		}
		find(st.Pred)
	}
	// 按日期的关联,参照 = 今天 / 现在: 对方的开始 / 结束列落在探针的边界上(数据来源的关联和嵌套里的关联)
	asOf := func(st core.Step) {
		if st.Kind != "lookup" || st.AsOf == nil || !isTimeExpr(st.AsOf.Ref) {
			return
		}
		for _, c := range []string{st.AsOf.Start, st.AsOf.End} {
			if c == "" {
				continue
			}
			t, _ := core.Schema(st.Columns).Col(c)
			kind := t.Kind
			if st.AsOf.YMD { // 整数编码的日期: 边界是日期的整数(约 1/4 不合法)
				kind = core.Date
			}
			e.tm.cmps = append(e.tm.cmps, derivedCmp{table: st.Table, step: -1, col: c, expr: st.AsOf.Ref, kind: kind, asof: true, ymd: st.AsOf.YMD})
		}
		walkIn(st.AsOf.Ref)
	}
	for _, st := range p.Steps {
		asOf(st)
		for _, s := range st.Source {
			asOf(s)
		}
	}
	if len(e.tm.cmps) == 0 {
		return
	}
	for _, d := range p.Params {
		if inputs[d.Name] {
			e.tm.inputs = append(e.tm.inputs, d)
		}
	}
	h := fnv.New64a()
	h.Write([]byte("probes " + e.api.ID + os.Getenv("FACTORY_DIFF_SEED")))
	rnd := rand.New(rand.NewSource(int64(h.Sum64())))
	nows := e.nowCandidates()
	cands := map[string][]any{}
	for _, d := range e.tm.inputs {
		cands[d.Name] = e.inputCandidates(d)
	}
	// 探针: 候选的全部组合(不超过 32 组时);更多时前几组按顺序轮一遍(每个候选都用到),之后随机
	var keys []string
	var lists [][]any
	if e.tm.usesNow {
		keys, lists = append(keys, core.NowParam), append(lists, nows)
	}
	for _, d := range e.tm.inputs {
		if len(cands[d.Name]) > 0 {
			keys, lists = append(keys, d.Name), append(lists, cands[d.Name])
		}
	}
	total := 1
	for _, l := range lists {
		total *= len(l)
	}
	const maxProbes = 32
	for i := 0; i < min(total, maxProbes); i++ {
		ps := core.Params{}
		rest := i
		for j, l := range lists {
			k := rest % len(l) // 全部组合: 混合进制
			rest /= len(l)
			if total > maxProbes {
				k = pickIndex(rnd, i, len(l))
			}
			ps[keys[j]] = l[k]
		}
		e.tm.probes = append(e.tm.probes, ps)
	}
}

// pickIndex: 前几轮按顺序轮一遍(每个候选都用到),之后随机。
func pickIndex(rnd *rand.Rand, i, n int) int {
	if i < n {
		return i
	}
	return rnd.Intn(n)
}

// nowCandidates: now 的候选: 值域的中心(UTC 0 点)、领域时区的日界线 −1 秒 / 正好 / +1 秒、月界线前后、普通的时刻。
func (e *diffEnv) nowCandidates() []any {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if t, ok := kinds.ByCore(core.Timestamp); ok && len(t.Domain) > 0 {
		base = t.Domain[len(t.Domain)/2].(time.Time)
	}
	loc := time.UTC
	if l, err := time.LoadLocation(e.tm.zone); err == nil && e.tm.zone != "" {
		loc = l
	}
	y, m, d := base.In(loc).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, loc).UTC()
	monthEnd := time.Date(y, m+1, 1, 0, 0, 0, 0, loc).UTC() // 下个月 1 日 0 点(领域时区)
	// 日界线 / 月界线只在表达式里有天 / 月的运算时才有意义(候选少一些,每组探针分到的数据多一些)
	day, month := false, false
	for _, c := range e.tm.cmps {
		unitsIn(c.expr, &day, &month)
	}
	out := []any{base, base.Add(97 * time.Minute), base.Add(-11 * time.Hour)}
	if day || month {
		out = append(out, midnight.Add(-time.Second), midnight, midnight.Add(time.Second))
	}
	if month {
		out = append(out, monthEnd.Add(-time.Second), monthEnd)
	}
	return out
}

func unitsIn(x *core.Expr, day, month *bool) {
	if x == nil {
		return
	}
	if x.Op == "add" || x.Op == "trunc" {
		*day = *day || x.Name == core.Day
		*month = *month || x.Name == core.Month
	}
	if x.Op == "local_date" { // 今天: 领域时区的日界线前后
		*day = true
	}
	for _, a := range x.Args {
		unitsIn(a, day, month)
	}
}

// inputCandidates: 派生参数的输入的候选。整数 = 约束的值域;日期 = 月末(31 / 30 / 2 月末 / 闰年)+ 值域;时间 = 值域 + 日界线。
// 整数编码的日期: 日期的候选写成 YYYYMMDD + 一个不合法的日期(请求不合法: 参数错误)。
func (e *diffEnv) inputCandidates(d core.ParamDecl) []any {
	if e.tm.ymdIn[d.Name] {
		var out []any
		for _, v := range e.inputCandidates(core.ParamDecl{Name: "\x00", Type: core.Type{Kind: core.Date}}) {
			out = append(out, core.YMDInt(v.(time.Time)))
		}
		return append(out, int64(20260230))
	}
	switch d.Type.Kind {
	case core.Int32, core.Int64:
		if dom := constraints.DomainOf(e.api.Request.Field(d.Name)); dom != nil && len(dom.Values) > 0 {
			// 约束的边界值 + 区间里常见的几个值(5 / 30 / 60 / 90 分钟这类;区间外的不用,请求会不合法)
			lo, hi := dom.Values[0].(int64), dom.Values[0].(int64)
			for _, v := range dom.Values {
				lo, hi = min(lo, v.(int64)), max(hi, v.(int64))
			}
			out := append([]any(nil), dom.Values...)
			for _, v := range []int64{5, 30, 60, 90} {
				if v > lo && v < hi && v != lo+1 && v != lo+2 {
					out = append(out, v)
				}
			}
			return out
		}
		return []any{int64(1), int64(2), int64(5), int64(60)}
	case core.Date:
		var out []any
		for _, s := range []string{"2026-03-31", "2026-01-31", "2024-03-31", "2026-03-30", "2026-03-29", "2026-02-28", "2024-02-29", "2026-05-31", "2026-01-15", "2026-01-01", "2025-12-31"} {
			t, _ := time.Parse("2006-01-02", s)
			out = append(out, t)
		}
		return out
	case core.Timestamp:
		return e.nowCandidates()
	}
	return nil
}

// probeKeys: 探针里的参数名。
func (e *diffEnv) probeKeys() []string {
	var ks []string
	if e.tm.usesNow {
		ks = append(ks, core.NowParam)
	}
	for _, d := range e.tm.inputs {
		ks = append(ks, d.Name)
	}
	return ks
}

// complete: 请求里缺少探针的参数时补上一组(按请求本身的内容确定地选,同一个请求总是同一组)。
func (e *diffEnv) complete(ps core.Params) core.Params {
	if len(e.tm.probes) == 0 {
		return ps
	}
	missing := false
	for _, k := range e.probeKeys() {
		if _, ok := ps[k]; !ok {
			missing = true
		}
	}
	if !missing {
		return ps
	}
	keys := make([]string, 0, len(ps))
	for k := range ps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := fnv.New32a()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s;", k, core.Canonical(ps[k]))
	}
	return e.withProbe(ps, int(h.Sum32()%uint32(len(e.tm.probes))))
}

func (e *diffEnv) withProbe(ps core.Params, i int) core.Params {
	out := core.Params{}
	for k, v := range ps {
		out[k] = v
	}
	for k, v := range e.tm.probes[i%len(e.tm.probes)] {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// spreadProbes: 每个请求带一组探针;请求比探针少时重复请求,每组探针都至少用一次。
func (e *diffEnv) spreadProbes(reqs []core.Params) []core.Params {
	if len(e.tm.probes) == 0 || len(reqs) == 0 {
		return reqs
	}
	n := max(len(reqs), len(e.tm.probes), diffRequests)
	var out []core.Params
	seen := map[string]bool{}
	for k := 0; k < n; k++ {
		ps := e.withProbe(reqs[k%len(reqs)], k)
		if d := e.describe(ps); !seen[d] {
			seen[d] = true
			out = append(out, ps)
		}
	}
	return out
}

// boundaryValue: 和派生参数比较的列的一个值: 某个探针的边界 −1 / 正好 / +1(时间 ±1 秒,日期 ±1 天)。没有 → nil。
func (e *diffEnv) boundaryValue(col string) any {
	return e.boundaryOf(col)
}

// boundaryCol: 表 table 的列 col 和 今天 / 现在 / 派生参数比较(取边界的值)。
func (e *diffEnv) boundaryCol(table, col string) bool {
	for _, c := range e.tm.cmps {
		if c.col == col && c.table == table {
			return true
		}
	}
	return false
}

// boundaryOf: boundaryValue 的本体(e.table 的列 col)。
func (e *diffEnv) boundaryOf(col string) any {
	var cs []derivedCmp
	for _, c := range e.tm.cmps {
		if c.col == col && c.table == e.table { // e.table = 正在造数据的表(子表的数据也在这里)
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 || len(e.tm.probes) == 0 {
		return nil
	}
	c := cs[e.rnd.Intn(len(cs))]
	probe := e.tm.pin
	if probe == nil {
		probe = e.tm.probes[e.rnd.Intn(len(e.tm.probes))]
	}
	pl := &core.Pipeline{Params: e.api.Pipeline.Params, Steps: []core.Step{
		core.Scan("_", core.NamedType{Name: "_", Type: core.Type{Kind: core.Int64}}),
		core.Project(core.NamedExpr{Name: "v", Expr: c.expr}),
	}}
	res, err := core.Eval(pl, map[string][]core.Row{"_": {{"_": int64(1)}}}, probe)
	if err != nil || len(res.Rows) != 1 {
		return nil
	}
	b, ok := res.Rows[0]["v"].(time.Time)
	if !ok {
		return nil
	}
	off := []int{-1, 0, 0, 1}[e.rnd.Intn(4)] // 正好在边界上的行多一些(< 和 <= 只在这里不同)
	if c.ymd {                               // 整数编码的日期: 边界 −1 / 正好 / +1 天的整数;约 1/4 是边界那个月附近的不合法的整数
		// (那个月的 0 日 / 最后一天 + 1 / 13 月: 不检查就按日历进位的实现会把它们读成边界前后的日期)
		if e.rnd.Intn(4) == 0 {
			y, m := int64(b.Year()), int64(b.Month())
			last := int64(time.Date(b.Year(), b.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day())
			return []int64{y*10000 + m*100, y*10000 + m*100 + last + 1, y*10000 + 1300 + int64(b.Day())}[e.rnd.Intn(3)]
		}
		return core.YMDInt(b.AddDate(0, 0, off))
	}
	if c.kind == core.Date {
		return b.AddDate(0, 0, off)
	}
	return b.Add(time.Duration(off) * time.Second)
}

// timeTargets: 派生参数的覆盖条件(错误的实现要被 ≥3 个请求区分):
// 边界的 < ↔ <=(> ↔ >=)、截日 / 月加法用的时区换成 UTC、now 换成别的时刻(读错了时钟)。
func (e *diffEnv) timeTargets() []sensTarget {
	var out []sensTarget
	flip := map[string]string{"<": "<=", "<=": "<", ">": ">=", ">=": ">"}
	looseDone := map[string]bool{}
	for _, c := range e.tm.cmps {
		c := c
		if c.asof { // 按日期的关联的参照: difflookup.go 的 asOfTargets
			continue
		}
		if f, ok := flip[c.node.Name]; ok {
			out = append(out, sensTarget{desc: fmt.Sprintf("派生参数的边界(%s %s 写成 %s)", c.col, c.node.Name, f), mutate: func(p *core.Pipeline) *core.Pipeline {
				return replaceIn(p, c.step, c.node, &core.Expr{Op: "cmp", Name: f, Args: c.node.Args})
			}})
		}
		if zoneSensitive(c.expr, e) {
			out = append(out, sensTarget{desc: fmt.Sprintf("派生参数 %s 的时区(换成 UTC)", c.col), mutate: func(p *core.Pipeline) *core.Pipeline {
				return replaceIn(p, c.step, c.expr, rezone(c.expr, "UTC"))
			}})
		}
		if c.ymd && !looseDone[c.col] { // 整数编码的日期: 不合法的整数按日历进位(20260230 → 3/2)的实现要看得出来
			looseDone[c.col] = true // 一列一个: 这一列的全部比较一起换(期间的两端都按进位读,进位后的日期才会落在期间里)
			out = append(out, sensTarget{desc: fmt.Sprintf("整数编码的日期 %s: 不合法的值按日历进位", c.col), mutate: func(p *core.Pipeline) *core.Pipeline {
				mut := *p
				mut.Steps = append([]core.Step(nil), p.Steps...)
				for i := range mut.Steps {
					mut.Steps[i].Pred = looseYMD(mut.Steps[i].Pred, c.col)
				}
				return &mut
			}})
		}
		if core.UsesNow(c.expr) {
			out = append(out, sensTarget{desc: fmt.Sprintf("派生参数 %s 的 now(读错了时钟: 晚 1 天)", c.col), mutate: func(p *core.Pipeline) *core.Pipeline {
				return replaceIn(p, c.step, c.expr, shiftNow(c.expr))
			}})
		}
	}
	return out
}

// zoneSensitive: 表达式里有作用在时间上的 trunc / 天·月的 add,而且时区不是 UTC。
func zoneSensitive(x *core.Expr, e *diffEnv) bool {
	if x == nil {
		return false
	}
	if (x.Op == "trunc" || x.Op == "local_date" || (x.Op == "add" && (x.Name == core.Day || x.Name == core.Month))) && x.Zone != "" && x.Zone != "UTC" {
		if t, err := e.api.Pipeline.TypeOf(x.Args[0], e.cols); err == nil && t.Kind == core.Timestamp { // 日期的运算和时区无关
			return true
		}
	}
	for _, a := range x.Args {
		if zoneSensitive(a, e) {
			return true
		}
	}
	return false
}

func rezone(x *core.Expr, zone string) *core.Expr {
	c := *x
	if c.Op == "add" || c.Op == "trunc" || c.Op == "local_date" {
		c.Zone = zone
	}
	c.Args = nil
	for _, a := range x.Args {
		c.Args = append(c.Args, rezone(a, zone))
	}
	return &c
}

func shiftNow(x *core.Expr) *core.Expr {
	if x.Op == "now" {
		return core.Add(core.Now(), core.Const(int64(1), core.Type{Kind: core.Int32}), core.Day, false, "UTC")
	}
	c := *x
	c.Args = nil
	for _, a := range x.Args {
		c.Args = append(c.Args, shiftNow(a))
	}
	return &c
}

func replaceIn(p *core.Pipeline, step int, target, with *core.Expr) *core.Pipeline {
	mut := *p
	mut.Steps = append([]core.Step(nil), p.Steps...)
	mut.Steps[step].Pred = replaceExpr(p.Steps[step].Pred, target, with)
	return &mut
}

// presentInput: 派生参数的输入 → 请求里的表示(日期 "YYYY-MM-DD",时间 RFC 3339,整数原样)。ok = 这是派生参数的输入。
func (e *diffEnv) presentInput(name string, v any) (any, bool) {
	for _, d := range e.tm.inputs {
		if d.Name != name {
			continue
		}
		t, isTime := v.(time.Time)
		switch {
		case isTime && d.Type.Kind == core.Date:
			return t.Format("2006-01-02"), true
		case isTime:
			return t.UTC().Format(time.RFC3339Nano), true
		}
		return v, true
	}
	return nil, false
}

// nowText: 请求头 X-Factory-Now 的值。
func nowText(v any) string {
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// looseYMD: 条件里的 ymd(列 col) 换成按日历进位的错误实现。
func looseYMD(x *core.Expr, col string) *core.Expr {
	if x == nil {
		return nil
	}
	c := *x
	if x.Op == "ymd" && len(x.Args) == 1 && x.Args[0].Op == "col" && x.Args[0].Name == col {
		c.Loose = true
	}
	c.Args = nil
	for _, a := range x.Args {
		c.Args = append(c.Args, looseYMD(a, col))
	}
	return &c
}
