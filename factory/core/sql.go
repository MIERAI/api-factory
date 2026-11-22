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
	out      Schema
	refs     map[string]string // lookup 加的列 → SQL 里的写法(t2.name);其他列 = 别名.列
}

type sqlGen struct {
	ref    func(ParamDecl, int) string
	p      *Pipeline
	params []ParamDecl
	index  map[string]int
	n      int // 别名计数
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
			if lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != "" {
				lv = g.wrap(lv, false)
			}
			w, err := g.expr(st.Pred, lv)
			if err != nil {
				return nil, err
			}
			lv.where = append(lv.where, w)
		case "project":
			if lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != "" {
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
			if lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != "" {
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
		case "sort":
			if lv.project != nil || lv.distinct != nil || lv.order != nil || lv.limit != "" {
				lv = g.wrap(lv, false)
			}
			lv.order = st.Order
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
	} else {
		for _, c := range lv.out {
			e := g.col(lv, c.Name)
			switch {
			case textOutput:
				sel = append(sel, e+"::text AS "+quoteIdent(c.Name))
			case e != lv.alias+"."+quoteIdent(c.Name): // lookup 加的列: 改名
				sel = append(sel, e+" AS "+quoteIdent(c.Name))
			default:
				sel = append(sel, e)
			}
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

func (g *sqlGen) expr(e *Expr, lv *sqlLevel) (string, error) {
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
	}
	return "", fmt.Errorf("sql: unknown expr %q", e.Op)
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
