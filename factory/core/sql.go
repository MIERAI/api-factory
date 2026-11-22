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
// 参数名是 SQL 的关键字(limit、order …)或不是普通的标识符时写成文本('limit'): sqlc 的解析器把 sqlc.arg(limit) 当作语法错误。
func Sqlc(d ParamDecl, _ int) string {
	name := d.Name
	if quoteIdent(name) != name {
		name = quoteLit(name)
	}
	if d.Required || d.List {
		return "sqlc.arg(" + name + ")"
	}
	return "sqlc.narg(" + name + ")"
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
	lv, err := g.steps(p.Steps)
	if err != nil {
		return nil, err
	}
	text, err := g.render(lv, textOutput)
	if err != nil {
		return nil, err
	}
	return &SQL{Text: text, Params: g.params, Columns: out}, nil
}

// steps: 流水线的步骤 → 最后一层(还没有写成文本)。关联 / 子表的来源(Source)也用它写成子查询。
func (g *sqlGen) steps(steps []Step) (*sqlLevel, error) {
	var lv *sqlLevel
	for _, st := range steps {
		sp := stepPrims[st.Kind] // 每个元件的 SQL 在 prim_<kind>.go(Check 之后不会有不认识的元件)
		if sp == nil {
			continue
		}
		var err error
		if lv, err = sp.sql(g, lv, st); err != nil {
			return nil, err
		}
	}
	return lv, nil
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
	xp := exprPrims[e.Op] // 每个元件的 SQL 在 expr_<op>.go
	pos := g.pos
	if xp == nil || !xp.keepPos { // 肯定位置只穿过 and / or(not、比较的两边、函数的参数都不是)
		defer func() { g.pos = pos }()
		g.pos = false
	}
	if xp == nil || xp.sql == nil {
		return "", fmt.Errorf("sql: unknown expr %q", e.Op)
	}
	return xp.sql(g, e, lv, pos)
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

// reserved: Postgres 的关键字里不能直接当名字用的(保留的、列名 / 类型名 / 函数名专用的:
// SELECT word FROM pg_get_keywords() WHERE catcode <> 'U',Postgres 16)。列名、参数名是这些词时加引号。
var reserved = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true, "array": true, "as": true, "asc": true,
	"asymmetric": true, "authorization": true, "between": true, "bigint": true, "binary": true, "bit": true,
	"boolean": true, "both": true, "case": true, "cast": true, "char": true, "character": true, "check": true,
	"coalesce": true, "collate": true, "collation": true, "column": true, "concurrently": true, "constraint": true,
	"create": true, "cross": true, "current_catalog": true, "current_date": true, "current_role": true,
	"current_schema": true, "current_time": true, "current_timestamp": true, "current_user": true, "dec": true,
	"decimal": true, "default": true, "deferrable": true, "desc": true, "distinct": true, "do": true, "else": true,
	"end": true, "except": true, "exists": true, "extract": true, "false": true, "fetch": true, "float": true,
	"for": true, "foreign": true, "freeze": true, "from": true, "full": true, "grant": true, "greatest": true,
	"group": true, "grouping": true, "having": true, "ilike": true, "in": true, "initially": true, "inner": true,
	"inout": true, "int": true, "integer": true, "intersect": true, "interval": true, "into": true, "is": true,
	"isnull": true, "join": true, "json_array": true, "json_arrayagg": true, "json_object": true,
	"json_objectagg": true, "lateral": true, "leading": true, "least": true, "left": true, "like": true, "limit": true,
	"localtime": true, "localtimestamp": true, "national": true, "natural": true, "nchar": true, "none": true,
	"normalize": true, "not": true, "notnull": true, "null": true, "nullif": true, "numeric": true, "offset": true,
	"on": true, "only": true, "or": true, "order": true, "out": true, "outer": true, "overlaps": true, "overlay": true,
	"placing": true, "position": true, "precision": true, "primary": true, "real": true, "references": true,
	"returning": true, "right": true, "row": true, "select": true, "session_user": true, "setof": true, "similar": true,
	"smallint": true, "some": true, "substring": true, "symmetric": true, "system_user": true, "table": true,
	"tablesample": true, "then": true, "time": true, "timestamp": true, "to": true, "trailing": true, "treat": true,
	"trim": true, "true": true, "union": true, "unique": true, "user": true, "using": true, "values": true,
	"varchar": true, "variadic": true, "verbose": true, "when": true, "where": true, "window": true, "with": true,
	"xmlattributes": true, "xmlconcat": true, "xmlelement": true, "xmlexists": true, "xmlforest": true,
	"xmlnamespaces": true, "xmlparse": true, "xmlpi": true, "xmlroot": true, "xmlserialize": true, "xmltable": true,
}

func quoteIdent(s string) string {
	if plainIdent.MatchString(s) && !reserved[s] {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }
