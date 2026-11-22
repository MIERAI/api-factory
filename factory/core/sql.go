package core

// SQL 后端: 流水线 → 一条 Postgres SQL(参数 $1, $2 …,每个都带显式 cast)。
//
// 规则(每一条都在元件认证里被验证):
//   - 列引用一律带表别名(t1.col)。ORDER BY 里不带别名的名字会先匹配输出列的别名(曾经因此按 ::text 排序)
//   - 文本的大小比较和排序加 COLLATE "C"(按字节,与参考求值器一致;数据库默认的排序规则可能不是按字节)
//   - NULL 的位置用 Postgres 的默认(ASC NULLS LAST / DESC NULLS FIRST),与参考求值器一致
//   - 一层 SELECT 能装下就合并,装不下就把前面的包成子查询

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// SQL: 生成的查询。Params[i] 对应 $i+1。
type SQL struct {
	Text    string
	Params  []ParamDecl
	Columns Schema
}

type sqlLevel struct {
	from     string
	alias    string
	in       Schema // 这一层能看到的列
	where    []string
	project  []NamedExpr
	distinct []string
	dOrder   []SortKey
	order    []SortKey
	limit    string
	group    []string // GROUP BY 的键(group)
	aggs     []Agg
	rank     *Step // rank() OVER (ORDER BY …)
	out      Schema
	refs     map[string]string // lookup 加的列 → SQL 里的写法(t2.name);其他列 = 别名.列
}

// busy: 这一层已经有 SELECT 列表 / DISTINCT / ORDER BY / LIMIT / GROUP BY / 窗口函数 → 后面的步骤要包一层子查询。
func (lv *sqlLevel) busy() bool {
	return lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != "" || lv.group != nil || lv.rank != nil
}

type sqlGen struct {
	ref    func(ParamDecl, int) string
	p      *Pipeline
	params []ParamDecl
	index  map[string]int
	n      int // 别名计数
	// pos: 正在写的表达式在 WHERE 的肯定位置(只经过 and / or,不在 not 之下)。这里 UNKNOWN 和 FALSE 的效果一样
	// (都不留下这一行),ymd 的比较可以改写成整数列上的比较(见 ymdCmp)
	pos bool
}

// Options: SQL 的写法。
type Options struct {
	TextOutput bool // 输出列全部 ::text(元件认证用,比较时不依赖驱动的类型映射)
	// ParamRef: 参数的写法。nil = $1, $2 …;Sqlc = sqlc.arg(名字) / sqlc.narg(名字)
	ParamRef func(d ParamDecl, index int) string
}

// Sqlc: sqlc 的参数写法(必填 / 列表 → sqlc.arg,可选的标量 → sqlc.narg)。
func Sqlc(d ParamDecl, _ int) string {
	if d.Required || d.List {
		return "sqlc.arg(" + d.Name + ")"
	}
	return "sqlc.narg(" + d.Name + ")"
}

// ToSQL: 流水线 → SQL。
func ToSQL(p *Pipeline, opt Options) (*SQL, error) {
	textOutput := opt.TextOutput
	out, err := Check(p)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(p.Steps); i++ { // 反复下推到不变: 越过 API 的每组取一条之后,还能再越过来源的取最新(键是自然键的一部分时)
		q := pushdown(p)
		if q == p {
			break
		}
		if _, err := Check(q); err != nil {
			return nil, fmt.Errorf("筛选下推后的流水线不合法(工厂的 bug): %w", err)
		}
		p = q
	}
	g := &sqlGen{p: p, index: map[string]int{}, ref: opt.ParamRef}
	var lv *sqlLevel
	for _, st := range p.Steps {
		switch st.Kind {
		case "scan":
			lv = g.newLevel(quoteIdent(st.Table), Schema(st.Columns))
		case "filter":
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			w, err := g.where(st.Pred, lv)
			if err != nil {
				return nil, err
			}
			lv.where = append(lv.where, w)
		case "project":
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			lv.project = st.Project
			var s Schema
			for _, x := range st.Project {
				t, err := p.typeOf(x.Expr, lv.in)
				if err != nil {
					return nil, err
				}
				s = append(s, NamedType{x.Name, t})
			}
			lv.out = s
		case "pick_per_key":
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			lv.distinct, lv.dOrder = st.Keys, st.Order
		case "lookup": // LEFT JOIN(drop: JOIN)接在这一层的 FROM 后面。WHERE 只引用之前的列,和连接的先后无关,不用包
			if lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != "" {
				lv = g.wrap(lv, false)
			}
			g.n++
			alias := fmt.Sprintf("t%d", g.n)
			var conds []string
			for _, k := range st.On {
				conds = append(conds, fmt.Sprintf("%s = %s.%s", g.col(lv, k.Col), alias, quoteIdent(k.Key)))
			}
			join := "LEFT JOIN"
			if st.Missing == "drop" {
				join = "JOIN"
			}
			lv.from += fmt.Sprintf("\n%s %s AS %s ON %s", join, quoteIdent(st.Table), alias, strings.Join(conds, " AND "))
			if lv.refs == nil {
				lv.refs = map[string]string{}
			}
			for _, t := range st.Take {
				lv.refs[t.Name] = alias + "." + quoteIdent(t.Col)
			}
			lv.in = checkLookup(lv.in, st, "", func(string, ...any) {})
			lv.out = lv.in
		case "nest", "child_agg": // LEFT JOIN LATERAL (子查询) ON true 接在这一层的 FROM 后面(和 lookup 一样不用包)
			if lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != "" {
				lv = g.wrap(lv, false)
			}
			sub, err := g.childSQL(lv, st)
			if err != nil {
				return nil, err
			}
			g.n++
			alias := fmt.Sprintf("t%d", g.n)
			lv.from += fmt.Sprintf("\nLEFT JOIN LATERAL (\n%s\n) AS %s ON TRUE", indent(sub), alias)
			if lv.refs == nil {
				lv.refs = map[string]string{}
			}
			if st.Kind == "nest" {
				lv.refs[st.As] = alias + ".v"
				lv.in = p.checkNest(lv.in, st, "", func(string, ...any) {})
			} else {
				for _, a := range st.Aggs {
					lv.refs[a.Name] = alias + "." + quoteIdent(a.Name)
				}
				lv.in = p.checkChildAgg(lv.in, st, "", func(string, ...any) {})
			}
			lv.out = lv.in
		case "sort":
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			lv.order = st.Order
		case "group": // 同一层: SELECT 键, 聚合 FROM … WHERE … GROUP BY 键
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			out, err := groupSchema(lv.in, st)
			if err != nil {
				return nil, err
			}
			lv.group, lv.aggs, lv.out = st.Keys, st.Aggs, out
		case "rank": // 总是包一层: 窗口函数的 ORDER BY 引用 group 的输出列
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			r := st
			lv.rank = &r
			lv.out = append(append(Schema(nil), lv.in...), NamedType{st.As, Type{Kind: Int64}})
		case "limit":
			if lv.limit != "" {
				lv = g.wrap(lv, false)
			}
			if st.Param != "" {
				lv.limit, _ = g.param(st.Param)
			} else {
				lv.limit = fmt.Sprint(st.N)
			}
		}
	}
	text, err := g.render(lv, textOutput)
	if err != nil {
		return nil, err
	}
	return &SQL{Text: text, Params: g.params, Columns: out}, nil
}

// childSQL: nest / child_agg 的 LATERAL 子查询。子表的行: 关联条件(当前行的列 = 子表的列;任一边 NULL → 不相等)+ 子表上的条件。
//
//	nest:      SELECT coalesce(jsonb_agg(jsonb_build_object(…) ORDER BY …), '[]') AS v
//	           FROM (SELECT 用到的列 FROM 子表 WHERE … ORDER BY … LIMIT N+1) AS w
//	           (多取一行: 超过上限由出口判定;值按核心的 API 表示写进 JSON: jsonValueSQL)
//	child_agg: SELECT 聚合(agg.go 的 aggSQL) AS 名字, … FROM 子表 WHERE …(没有 GROUP BY: 永远一行,空集合也有)
func (g *sqlGen) childSQL(lv *sqlLevel, st Step) (string, error) {
	g.n++
	child := &sqlLevel{alias: fmt.Sprintf("t%d", g.n), in: Schema(st.Columns), out: Schema(st.Columns)}
	var where []string
	for _, k := range st.On {
		where = append(where, fmt.Sprintf("%s = %s", g.col(child, k.Key), g.col(lv, k.Col)))
	}
	if st.Pred != nil {
		w, err := g.where(st.Pred, child)
		if err != nil {
			return "", err
		}
		where = append(where, w)
	}
	from := fmt.Sprintf("FROM %s AS %s\nWHERE %s", quoteIdent(st.Table), child.alias, strings.Join(where, "\n  AND "))
	if st.Kind == "child_agg" {
		var sel []string
		for _, a := range st.Aggs {
			x, t := "", Type{}
			if a.Col != "" {
				x = g.col(child, a.Col)
				t, _ = child.in.Col(a.Col)
			}
			sel = append(sel, aggSQL(a, x, t)+" AS "+quoteIdent(a.Name))
		}
		return "SELECT " + strings.Join(sel, ", ") + "\n" + from, nil
	}
	// nest: 内层取用到的列(子行的列 + 顺序的列),外层按同样的顺序聚合成 JSON
	var cols []string
	seen := map[string]bool{}
	for _, c := range append(takeCols(st.Take), orderNames(st.Order)...) {
		if !seen[c] {
			seen[c] = true
			cols = append(cols, g.col(child, c))
		}
	}
	inner := fmt.Sprintf("SELECT %s\n%s\nORDER BY %s\nLIMIT %d", strings.Join(cols, ", "), from, strings.Join(g.orderItems(child, st.Order), ", "), st.N+1)
	g.n++
	w := &sqlLevel{alias: fmt.Sprintf("t%d", g.n), in: child.in, out: child.in}
	var obj []string
	for _, t := range st.Take {
		ct, _ := child.in.Col(t.Col)
		obj = append(obj, quoteLit(t.Name), jsonValueSQL(g.col(w, t.Col), ct.Kind))
	}
	return fmt.Sprintf("SELECT coalesce(jsonb_agg(jsonb_build_object(%s) ORDER BY %s), '[]'::jsonb) AS v\nFROM (\n%s\n) AS %s",
		strings.Join(obj, ", "), strings.Join(g.orderItems(w, st.Order), ", "), indent(inner), w.alias), nil
}

func takeCols(t []TakeCol) []string {
	var out []string
	for _, x := range t {
		out = append(out, x.Col)
	}
	return out
}

// jsonValueSQL: 子行的值在 JSON 里的写法 = 核心的 API 表示(参考求值器 ChildRowsFromJSON、适配器的扫描用同一种):
// 十进制 → 文本(numeric::text,不经过 JSON 数字: 不丢精度);时间 → UTC 的 RFC3339 文本(不受会话时区影响);
// 日期 → YYYY-MM-DD(不受 DateStyle 影响)。整数 / bool / 文本是 JSON 本来的类型。NULL → JSON null。
func jsonValueSQL(x string, k Kind) string {
	switch k {
	case Decimal:
		return x + "::text"
	case Date:
		return "to_char(" + x + ", 'YYYY-MM-DD')"
	case Timestamp:
		return "to_char(" + x + ` AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`
	}
	return x
}

func (g *sqlGen) newLevel(from string, in Schema) *sqlLevel {
	g.n++
	alias := fmt.Sprintf("t%d", g.n)
	return &sqlLevel{from: from + " AS " + alias, alias: alias, in: in, out: in}
}

func (g *sqlGen) wrap(lv *sqlLevel, _ bool) *sqlLevel {
	sub, _ := g.render(lv, false)
	return g.newLevel("(\n"+indent(sub)+"\n)", lv.out)
}

func (g *sqlGen) render(lv *sqlLevel, textOutput bool) (string, error) {
	var sel []string
	if lv.project != nil {
		for _, x := range lv.project {
			e, err := g.expr(x.Expr, lv)
			if err != nil {
				return "", err
			}
			if textOutput {
				e = "(" + e + ")::text"
			}
			sel = append(sel, e+" AS "+quoteIdent(x.Name))
		}
	} else if lv.group != nil { // 键 + 聚合(聚合的规则在 agg.go)
		for _, k := range g.groupKeys(lv) {
			e := k.expr
			if textOutput {
				e = "(" + e + ")::text"
			}
			sel = append(sel, e+" AS "+k.name)
		}
		for _, a := range lv.aggs {
			col, _ := lv.in.Col(a.Col)
			e := aggSQL(a, g.col(lv, a.Col), col)
			if textOutput {
				e = "(" + e + ")::text"
			}
			sel = append(sel, e+" AS "+quoteIdent(a.Name))
		}
	} else {
		cols := lv.out
		if lv.rank != nil { // 输入的列 + 名次
			cols = lv.in
		}
		for _, c := range cols {
			e := g.col(lv, c.Name) // lookup 加的列在别的表别名下(refs)
			switch {
			case textOutput:
				sel = append(sel, e+"::text AS "+quoteIdent(c.Name))
			case e != lv.alias+"."+quoteIdent(c.Name): // lookup 加的列: 改名
				sel = append(sel, e+" AS "+quoteIdent(c.Name))
			default:
				sel = append(sel, e)
			}
		}
		if lv.rank != nil { // NULL 的位置 / 文本按字节: 和 sort 同样的写法(orderItems)
			e := "rank() OVER (ORDER BY " + strings.Join(g.orderItems(lv, lv.rank.Order), ", ") + ")"
			if textOutput {
				e = "(" + e + ")::text"
			}
			sel = append(sel, e+" AS "+quoteIdent(lv.rank.As))
		}
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	if lv.distinct != nil {
		fmt.Fprintf(&b, "DISTINCT ON (%s) ", strings.Join(g.distinctKeys(lv), ", "))
	}
	b.WriteString(strings.Join(sel, ", "))
	b.WriteString("\nFROM " + lv.from)
	if len(lv.where) > 0 {
		b.WriteString("\nWHERE " + strings.Join(lv.where, "\n  AND "))
	}
	if lv.group != nil {
		var ks []string
		for _, k := range g.groupKeys(lv) {
			ks = append(ks, k.expr)
		}
		b.WriteString("\nGROUP BY " + strings.Join(ks, ", "))
	}
	var order []string
	if lv.distinct != nil {
		order = append(order, g.distinctKeys(lv)...)
		order = append(order, g.orderItems(lv, lv.dOrder)...)
	}
	order = append(order, g.orderItems(lv, lv.order)...)
	if len(order) > 0 {
		b.WriteString("\nORDER BY " + strings.Join(order, ", "))
	}
	if lv.limit != "" {
		b.WriteString("\nLIMIT " + lv.limit)
	}
	return b.String(), nil
}

type groupKey struct{ expr, name string }

// groupKeys: GROUP BY 的键。文本键用 COLLATE "C"(按字节,和参考求值器一致;SELECT 里写同一个表达式)。
// NULL 自己是一组(GROUP BY 的语义)。
func (g *sqlGen) groupKeys(lv *sqlLevel) []groupKey {
	var out []groupKey
	for _, k := range lv.group {
		e := g.col(lv, k) // 分组键也可以是 lookup 加的列
		if t, _ := lv.in.Col(k); t.Kind == Text {
			e += ` COLLATE "C"`
		}
		out = append(out, groupKey{e, quoteIdent(k)})
	}
	return out
}

// distinctKeys: 每组取一条的分组键。文本键也用 COLLATE "C"(分组不受影响: 确定性排序规则的相等就是按字节相等),
// 这样组的顺序和后面按键排序(COLLATE "C")一致,配上表达式索引,数据库可以边读索引边输出,读够 LIMIT 就停。
func (g *sqlGen) distinctKeys(lv *sqlLevel) []string {
	var ks []string
	for _, k := range lv.distinct {
		e := g.col(lv, k)
		if t, _ := lv.in.Col(k); t.Kind == Text {
			e += ` COLLATE "C"`
		}
		ks = append(ks, e)
	}
	return ks
}

func (g *sqlGen) orderItems(lv *sqlLevel, order []SortKey) []string {
	var out []string
	for _, o := range order {
		e := g.col(lv, o.Col)
		if t, _ := lv.in.Col(o.Col); t.Kind == Text {
			e += ` COLLATE "C"`
		}
		if o.Desc {
			e += " DESC"
		}
		out = append(out, e)
	}
	return out
}

// col: 列在这一层的写法。一律带表别名(lookup 加的列 = 对方的表的别名.列)。
func (g *sqlGen) col(lv *sqlLevel, name string) string {
	if r, ok := lv.refs[name]; ok {
		return r
	}
	return lv.alias + "." + quoteIdent(name)
}

func (g *sqlGen) param(name string) (string, *ParamDecl) {
	d := g.p.Param(name)
	if d == nil && name == NowParam { // now(): 不是请求参数,由适配器接到运行时的时钟
		d = &ParamDecl{Name: NowParam, Type: Type{Kind: Timestamp}, Required: true}
	}
	i, ok := g.index[name]
	if !ok {
		g.params = append(g.params, *d)
		i = len(g.params)
		g.index[name] = i
	}
	cast := d.Type.Kind.SQLType()
	if d.List {
		cast += "[]"
	}
	ref := fmt.Sprintf("$%d", i)
	if g.ref != nil {
		ref = g.ref(*d, i)
	}
	return ref + "::" + cast, d
}

// where: WHERE 里的条件(肯定位置)。
func (g *sqlGen) where(e *Expr, lv *sqlLevel) (string, error) {
	saved := g.pos
	g.pos = true
	defer func() { g.pos = saved }()
	return g.expr(e, lv)
}

func (g *sqlGen) expr(e *Expr, lv *sqlLevel) (string, error) {
	if e.Op != "and" && e.Op != "or" { // 肯定位置只穿过 and / or(not、比较的两边、函数的参数都不是)
		saved := g.pos
		defer func() { g.pos = saved }()
		if e.Op == "cmp" && saved && len(e.Args) == 2 && e.Args[0].Op == "ymd" {
			g.pos = false
			return g.ymdCmp(e, lv)
		}
		g.pos = false
	}
	sub := func(x *Expr) (string, error) { return g.expr(x, lv) }
	switch e.Op {
	case "col":
		return g.col(lv, e.Name), nil
	case "const":
		return constSQL(e.Value, e.Type.Kind)
	case "param":
		s, _ := g.param(e.Name)
		return s, nil
	case "cmp":
		a, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		b, err := sub(e.Args[1])
		if err != nil {
			return "", err
		}
		t, _ := g.p.typeOf(e.Args[0], lv.in)
		if t.Kind == Text && e.Name != "=" && e.Name != "<>" {
			a += ` COLLATE "C"`
		}
		return fmt.Sprintf("(%s %s %s)", a, e.Name, b), nil
	case "and", "or":
		var xs []string
		for _, x := range e.Args {
			s, err := sub(x)
			if err != nil {
				return "", err
			}
			if s == "FALSE" && e.Op == "or" { // 化简: 必填参数的 absent 恒为 FALSE
				continue
			}
			xs = append(xs, s)
		}
		switch len(xs) {
		case 0:
			return "FALSE", nil
		case 1:
			return xs[0], nil
		}
		return "(" + strings.Join(xs, " "+strings.ToUpper(e.Op)+" ") + ")", nil
	case "not":
		s, err := sub(e.Args[0])
		return "(NOT " + s + ")", err
	case "is_null":
		s, err := sub(e.Args[0])
		return "(" + s + " IS NULL)", err
	case "absent":
		if g.p.Param(e.Name).Required { // 必填: 恒为 FALSE(不登记参数,否则 SQL 里有用不到的 $n)
			return "FALSE", nil
		}
		p, d := g.param(e.Name)
		switch {
		case d.List:
			return fmt.Sprintf("(coalesce(cardinality(%s), 0) = 0)", p), nil
		}
		return "(" + p + " IS NULL)", nil
	case "in_list":
		a, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		p, _ := g.param(e.Name)
		return fmt.Sprintf("(%s = ANY(%s))", a, p), nil
	case "starts_with":
		a, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		p, _ := g.param(e.Name)
		return fmt.Sprintf(`(%s LIKE replace(replace(replace(%s, '\', '\\'), '%%', '\%%'), '_', '\_') || '%%')`, a, p), nil
	case "contains": // 前后都是 %: 和 starts_with 同样的转义(按字面)。LIKE 而不是 strpos: pg_trgm 的 GIN 索引能用
		a, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		p, _ := g.param(e.Name)
		return fmt.Sprintf(`(%s LIKE '%%' || replace(replace(replace(%s, '\', '\\'), '%%', '\%%'), '_', '\_') || '%%')`, a, p), nil
	case "json_field":
		a, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		v := fmt.Sprintf("(%s ->> %s)", a, quoteLit(e.Name))
		f, ok := jsonFormats[e.Type.Kind]
		if !ok {
			return fmt.Sprintf("(%s::%s)", v, e.Type.Kind.SQLType()), nil
		}
		// 不是规范写法 → 拼成必然转换失败的文本,查询失败(和参考求值器一致;NULL 仍然是 NULL)
		op := "~"
		if f.ci {
			op = "~*"
		}
		return fmt.Sprintf("((CASE WHEN %s %s %s THEN %s ELSE '✗' || %s END)::%s)", v, op, quoteLit(f.pattern), v, v, e.Type.Kind.SQLType()), nil
	case "tuple_after":
		var cols, ps []string
		var first string
		for i, c := range e.Args {
			s, err := sub(c)
			if err != nil {
				return "", err
			}
			if t, _ := g.p.typeOf(c, lv.in); t.Kind == Text {
				s += ` COLLATE "C"`
			}
			cols = append(cols, s)
			p, _ := g.param(e.Params[i])
			if i == 0 {
				first = p
			}
			ps = append(ps, p)
		}
		op := ">"
		if e.Desc {
			op = "<"
		}
		return fmt.Sprintf("(%s IS NULL OR (%s) %s (%s))", first, strings.Join(cols, ", "), op, strings.Join(ps, ", ")), nil
	case "in_tuples": // 并行的数组按下标对齐成组(单参数 unnest + WITH ORDINALITY);EXISTS 是两值的(和参考求值器一致)
		var from, conds []string
		for i, c := range e.Args {
			s, err := sub(c)
			if err != nil {
				return "", err
			}
			p, _ := g.param(e.Params[i])
			from = append(from, fmt.Sprintf("unnest(%s) WITH ORDINALITY AS tup%d(v, n)", p, i+1))
			conds = append(conds, fmt.Sprintf("%s = tup%d.v", s, i+1))
			if i > 0 {
				conds = append(conds, fmt.Sprintf("tup%d.n = tup1.n", i+1))
			}
		}
		return fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE %s)", strings.Join(from, ", "), strings.Join(conds, " AND ")), nil
	case "now":
		s, _ := g.param(NowParam)
		return s, nil
	case "ymd":
		x, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		// 外层的 CASE 先确认年月的范围,内层才用 make_date(不合法的月不会传给 make_date;Postgres 的 CASE 化简不会先算没有走到的分支)
		return fmt.Sprintf("(CASE WHEN %s THEN make_date((%s / 10000)::int4, (%s / 100 %% 100)::int4, (%s %% 100)::int4) END)", ymdValidSQL(x), x, x, x), nil
	case "add":
		x, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		n, err := g.intArg(e.Args[1], lv)
		if err != nil {
			return "", err
		}
		t, _ := g.p.typeOf(e.Args[0], lv.in)
		sign := "+"
		if e.Neg {
			sign = "-"
		}
		field := map[string]string{Minute: "mins", Hour: "hours", Day: "days", Month: "months"}[e.Name]
		iv := fmt.Sprintf("make_interval(%s => %s)", field, n)
		switch {
		case t.Kind == Date && e.Name == Day: // date ± int → date
			return fmt.Sprintf("(%s %s %s)", x, sign, n), nil
		case t.Kind == Date: // date ± interval → timestamp(没有时区)→ 日期
			return fmt.Sprintf("((%s %s %s)::date)", x, sign, iv), nil
		case e.Name == Minute || e.Name == Hour: // 绝对时长(和时区无关)
			return fmt.Sprintf("(%s %s %s)", x, sign, iv), nil
		}
		// 天 / 月: 在领域的时区的日历上加(不受会话时区影响)
		z := quoteLit(zoneName(e.Zone))
		return fmt.Sprintf("(((%s AT TIME ZONE %s) %s %s) AT TIME ZONE %s)", x, z, sign, iv, z), nil
	case "trunc":
		x, err := sub(e.Args[0])
		if err != nil {
			return "", err
		}
		t, _ := g.p.typeOf(e.Args[0], lv.in)
		if t.Kind == Date { // timestamp(没有时区)上截: 不受会话时区影响
			return fmt.Sprintf("(date_trunc(%s, (%s)::timestamp)::date)", quoteLit(e.Name), x), nil
		}
		z := quoteLit(zoneName(e.Zone))
		return fmt.Sprintf("(date_trunc(%s, (%s AT TIME ZONE %s)) AT TIME ZONE %s)", quoteLit(e.Name), x, z, z), nil
	}
	return "", fmt.Errorf("sql: unknown expr %q", e.Op)
}

func zoneName(z string) string {
	if z == "" {
		return "UTC"
	}
	return z
}

// intArg: add 的个数(make_interval 的参数是 int4)。常量直接写数字;int8 的表达式 cast 成 int4(超出范围 → 查询失败,和参考求值器一致)。
func (g *sqlGen) intArg(e *Expr, lv *sqlLevel) (string, error) {
	if e.Op == "const" {
		if v, ok := e.Value.(int64); ok {
			return fmt.Sprintf("(%d)", v), nil
		}
	}
	s, err := g.expr(e, lv)
	if err != nil {
		return "", err
	}
	if t, _ := g.p.typeOf(e, lv.in); t.Kind == Int64 {
		s = "(" + s + ")::int4"
	}
	return s, nil
}

func constSQL(v any, k Kind) (string, error) {
	if v == nil {
		return "NULL::" + k.SQLType(), nil
	}
	switch x := v.(type) {
	case int64:
		return fmt.Sprintf("%d::%s", x, k.SQLType()), nil
	case string:
		return quoteLit(x) + "::" + k.SQLType(), nil
	case bool:
		if x {
			return "TRUE", nil
		}
		return "FALSE", nil
	case *big.Rat:
		return quoteLit(RatString(x)) + "::numeric", nil
	case time.Time:
		if k == Date {
			return quoteLit(x.Format("2006-01-02")) + "::date", nil
		}
		return quoteLit(x.UTC().Format(time.RFC3339Nano)) + "::timestamptz", nil
	}
	return "", fmt.Errorf("sql: unsupported const %T", v)
}

var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

var reserved = map[string]bool{"from": true, "to": true, "order": true, "group": true, "select": true, "where": true,
	"user": true, "table": true, "limit": true, "offset": true, "desc": true, "asc": true, "end": true, "default": true}

func quoteIdent(s string) string {
	if plainIdent.MatchString(s) && !reserved[s] {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }

// ymdValidSQL: 整数 x 是合法的 YYYYMMDD(年 1000~9999、月 1~12、日 1~那个月的天数)。不会报错: 月不合法时不算那个月的天数。
func ymdValidSQL(x string) string {
	return fmt.Sprintf("(CASE WHEN %s BETWEEN 10000101 AND 99991231 AND %s / 100 %% 100 BETWEEN 1 AND 12 THEN %s %% 100 BETWEEN 1 AND "+
		"extract(day from make_date((%s / 10000)::int4, (%s / 100 %% 100)::int4, 1) + interval '1 month' - interval '1 day')::int4 ELSE FALSE END)", x, x, x, x, x)
}

// ymdCmp: 肯定位置的 cmp(op, ymd(x), v) → (x op <v 的 YYYYMMDD>) AND <x 合法>。
// 合法的 YYYYMMDD 的整数顺序 = 日期的顺序(年 1000~9999 都是 8 位),所以合法的 x 上两者相同;
// 不合法的 x: 原来是 UNKNOWN,改写后是 FALSE —— 在肯定位置效果一样(不留下这一行)。v 为 NULL → 两边都不是 TRUE。
// 比较按 x 本来的编码(整数)进行: x 是列时,列上的 btree 索引能用(v 的整数在执行前就算好了)。
func (g *sqlGen) ymdCmp(e *Expr, lv *sqlLevel) (string, error) {
	x, err := g.expr(e.Args[0].Args[0], lv)
	if err != nil {
		return "", err
	}
	v, err := g.expr(e.Args[1], lv)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("((%s %s (to_char(%s, 'YYYYMMDD'))::int4) AND %s)", x, e.Name, v, ymdValidSQL(x)), nil
}
