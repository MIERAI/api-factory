package core

// 关联的元件(lookup / nest / child_agg)共用的部分: 对方是逻辑表(Source)时的检查、求值、SQL,
// 以及 nest / child_agg 的子表(关联条件 + 子表上的条件)。
//
// lookup / nest / child_agg 的对方可以是逻辑表(Source 不为空): 对方的行 = Source 这条子流水线的结果
// (来源单元的展开: scan 物理表 → project → filter → pick_per_key → …;只能用这几种元件,不能引用请求参数)。
// Columns = 子流水线的输出的列,Table = 逻辑表名(报错用)。逻辑表的唯一键 = 子流水线的 pick_per_key 的键(自然键)。
// 其他规则(NULL 不相等、找不到、上限、全序)和普通的表一样。nest 的全序: 唯一键就是取最新的键时,
// 可以为空的列也算(NULL 自己是一组,一个父行里最多一行)。
// SQL 后端: LATERAL 子查询,关联条件里对方的列是取最新的键时,下推到取最新之前(索引能用;等式见 pushdown.go)

import (
	"fmt"
	"strings"
)

// checkChild: nest / child_agg 共同的部分: 子表、关联条件(当前行的列 = 子表的列)、子表上的条件。
func (p *Pipeline) checkChild(cur Schema, st Step, at string, add func(string, ...any)) (child Schema, ok bool) {
	child = Schema(st.Columns)
	if st.Table == "" || len(st.Columns) == 0 {
		add("%s: 需要子表和它的列", at)
		return child, false
	}
	if len(st.On) == 0 {
		add("%s: 需要关联条件(on)", at)
	}
	seen := map[string]bool{}
	for _, k := range st.On {
		a, okA := cur.Col(k.Col)
		b, okB := child.Col(k.Key)
		switch {
		case !okA:
			add("%s: 关联的列 %s 不存在", at, k.Col)
		case !okB:
			add("%s: %s 没有列 %s", at, st.Table, k.Key)
		case !comparable(a.Kind, b.Kind):
			add("%s: 关联条件 %s = %s.%s: %s 和 %s 不能比较", at, k.Col, st.Table, k.Key, a.Kind, b.Kind)
		}
		if seen[k.Key] {
			add("%s: 子表的列 %s 重复", at, k.Key)
		}
		seen[k.Key] = true
	}
	if st.Pred != nil {
		t, err := p.typeOf(st.Pred, child)
		if err != nil {
			add("%s: 子表的条件: %v", at, err)
		} else if t.Kind != Bool {
			add("%s: 子表的条件必须是 bool,实际是 %s", at, t.Kind)
		}
	}
	return child, true
}

// checkSource: lookup / nest / child_agg 的对方是逻辑表(Source)时: 子流水线本身合法、只用来源的元件、
// 不引用请求参数,输出的列和 Columns 一致(名字、类型;Columns 说不为空的列子流水线里也不为空)。
func (p *Pipeline) checkSource(st Step, at string, add func(string, ...any)) {
	if len(st.Source) == 0 {
		return
	}
	for _, s := range st.Source {
		switch s.Kind {
		case "scan", "project", "filter", "pick_per_key":
		default:
			add("%s: 对方的来源里不能有 %s(只能是 scan / project / filter / pick_per_key)", at, s.Kind)
			return
		}
		for _, x := range s.Project {
			if refersRequest(x.Expr) {
				add("%s: 对方的来源不能引用请求参数", at)
				return
			}
		}
		if refersRequest(s.Pred) {
			add("%s: 对方的来源不能引用请求参数", at)
			return
		}
	}
	sub, err := Check(&Pipeline{Steps: st.Source, Params: p.Params})
	if err != nil {
		add("%s: 对方的来源: %v", at, err)
		return
	}
	if len(sub) != len(st.Columns) {
		add("%s: 对方的列(%d 列)和来源的输出(%d 列)不一致", at, len(st.Columns), len(sub))
		return
	}
	for i, c := range st.Columns {
		s := sub[i]
		if s.Name != c.Name || s.Type.Kind != c.Type.Kind || (s.Type.Nullable && !c.Type.Nullable) {
			add("%s: 对方的列 %s %s(可空 %v)和来源的输出 %s %s(可空 %v)不一致", at, c.Name, c.Type.Kind, c.Type.Nullable, s.Name, s.Type.Kind, s.Type.Nullable)
		}
	}
}

// SourceKeys: 来源(子流水线)的取最新的键在输出里的列名(最后一个 pick_per_key 的键,跟着之后的改名;被丢掉 → nil)。
func SourceKeys(steps []Step) []string {
	var keys []string
	for _, s := range steps {
		switch s.Kind {
		case "pick_per_key":
			keys = append([]string(nil), s.Keys...)
		case "project":
			if keys == nil {
				continue
			}
			next := make([]string, len(keys))
			for i, k := range keys {
				for _, x := range s.Project {
					if x.Expr.Op == "col" && x.Expr.Name == k {
						next[i] = x.Name
						break
					}
				}
				if next[i] == "" {
					return nil
				}
			}
			keys = next
		}
	}
	return keys
}

func sameSet(a, b []string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

// related: lookup / nest / child_agg 的对方的行。逻辑表(Source)= 子流水线在同一组物理表上的结果。
func (p *Pipeline) related(st Step, tables map[string][]Row, params Params) ([]Row, error) {
	if len(st.Source) == 0 {
		return tables[st.Table], nil
	}
	res, err := Eval(&Pipeline{Steps: st.Source, Params: p.Params}, tables, params)
	if err != nil {
		return nil, fmt.Errorf("%s 的来源: %w", st.Table, err)
	}
	return res.Rows, nil
}

// childKey: 关联的键(规范文本)。任一列为 NULL → 没有键(NULL 和什么都不相等)。
func childKey(r Row, on []JoinKey, side func(JoinKey) string) (string, bool) {
	var b strings.Builder
	for _, k := range on {
		v := r[side(k)]
		if v == nil {
			return "", false
		}
		b.WriteString(Canonical(v))
		b.WriteString("\x00")
	}
	return b.String(), true
}

// childIndex: nest / child_agg 的子表的行,按关联的键建索引(键里有 NULL 的行不进索引),先过滤 Pred(UNKNOWN 丢弃)。
func (p *Pipeline) childIndex(st Step, child []Row, params Params) (map[string][]Row, error) {
	childSchema := Schema(st.Columns)
	index := map[string][]Row{}
	for _, c := range child {
		if st.Pred != nil {
			v, err := p.eval(st.Pred, c, childSchema, params)
			if err != nil {
				return nil, err
			}
			if v != true {
				continue
			}
		}
		if k, ok := childKey(c, st.On, func(j JoinKey) string { return j.Key }); ok {
			index[k] = append(index[k], c)
		}
	}
	return index, nil
}

// childGroup: 父行 r 关联的子行(childIndex 里的)。父行的关联列有 NULL → 没有子行。
func childGroup(r Row, st Step, index map[string][]Row) []Row {
	var group []Row
	if k, ok := childKey(r, st.On, func(j JoinKey) string { return j.Col }); ok {
		group = append(group, index[k]...)
	}
	return group
}

// joinBusy: lookup / nest / child_agg 接在这一层的 FROM 后面之前,要不要先包一层子查询。
// WHERE 只引用之前的列,和连接的先后无关,不用包(group / rank 之后也接得上: 不在这里检查,和以前一样)。
func (lv *sqlLevel) joinBusy() bool {
	return lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != ""
}

// sourceSQL: 逻辑表(st.Source)+ 关联条件(对方的列 = 外层 lv 的列)的子查询(LATERAL 里用)。
// 关联条件作为最后一步的筛选接在来源之后,再由 pushdown 把只引用取最新的键的条件移到取最新之前
// (等式: filter(P) ∘ pick_per_key(K) = pick_per_key(K) ∘ filter(P),P 只引用 K)—— 数据库只给关联到的键取最新,
// 用得上 (键, 登记顺序) 的索引;其余的条件留在取最新之后(语义不变)。外层的列写成 sql_ref(SQL 的原样文本)。
func (g *sqlGen) sourceSQL(st Step, lv *sqlLevel) (string, error) {
	steps := append([]Step(nil), st.Source...)
	var conds []*Expr
	for _, k := range st.On {
		t, _ := lv.in.Col(k.Col)
		conds = append(conds, Cmp("=", Col(k.Key), &Expr{Op: "sql_ref", Name: g.col(lv, k.Col), Type: &Type{Kind: t.Kind, Nullable: t.Nullable}}))
	}
	switch len(conds) {
	case 0:
	case 1:
		steps = append(steps, Step{Kind: "filter", Pred: conds[0], From: "关联条件"})
	default:
		steps = append(steps, Step{Kind: "filter", Pred: And(conds...), From: "关联条件"})
	}
	sub := &Pipeline{Steps: steps, Params: g.p.Params}
	for i := 0; i < len(sub.Steps); i++ {
		q := pushdown(sub)
		if q == sub {
			break
		}
		sub = q
	}
	saved := g.p
	g.p = sub
	defer func() { g.p = saved }()
	slv, err := g.steps(sub.Steps)
	if err != nil {
		return "", err
	}
	return g.render(slv, false)
}

// lateral: nest / child_agg: LEFT JOIN LATERAL (子查询) ON true 接在这一层的 FROM 后面(和 lookup 一样不用包)。
// 返回接上之后的层和子查询的别名(调用方登记加的列: lv.refs)。
func (g *sqlGen) lateral(lv *sqlLevel, st Step, sub func(*sqlLevel, Step) (string, error)) (*sqlLevel, string, error) {
	if lv.joinBusy() {
		lv = g.wrap(lv, false)
	}
	s, err := sub(lv, st)
	if err != nil {
		return nil, "", err
	}
	g.n++
	alias := fmt.Sprintf("t%d", g.n)
	lv.from += fmt.Sprintf("\nLEFT JOIN LATERAL (\n%s\n) AS %s ON TRUE", indent(s), alias)
	if lv.refs == nil {
		lv.refs = map[string]string{}
	}
	return lv, alias, nil
}

// childFrom: nest / child_agg 的 LATERAL 子查询的 FROM … WHERE …。子表的行: 关联条件(当前行的列 = 子表的列;
// 任一边 NULL → 不相等)+ 子表上的条件。返回子表这一层(列的写法)和 FROM 子句。
func (g *sqlGen) childFrom(lv *sqlLevel, st Step) (*sqlLevel, string, error) {
	table := quoteIdent(st.Table)
	if len(st.Source) > 0 { // 逻辑表: 来源 + 关联条件(下推到取最新之前)的子查询
		sub, err := g.sourceSQL(st, lv)
		if err != nil {
			return nil, "", err
		}
		table = "(\n" + indent(sub) + "\n)"
	}
	g.n++
	child := &sqlLevel{alias: fmt.Sprintf("t%d", g.n), in: Schema(st.Columns), out: Schema(st.Columns)}
	var where []string
	for _, k := range st.On {
		if len(st.Source) > 0 { // 已经在子查询里
			break
		}
		where = append(where, fmt.Sprintf("%s = %s", g.col(child, k.Key), g.col(lv, k.Col)))
	}
	if st.Pred != nil {
		w, err := g.where(st.Pred, child)
		if err != nil {
			return nil, "", err
		}
		where = append(where, w)
	}
	from := fmt.Sprintf("FROM %s AS %s", table, child.alias)
	if len(where) > 0 {
		from += "\nWHERE " + strings.Join(where, "\n  AND ")
	}
	return child, from, nil
}
