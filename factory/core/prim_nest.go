package core

// nest: 一对多嵌套: 对每一行,取 Table(列 = Columns)里 On 的列逐个相等的行(任一边 NULL → 不相等),
// 经过 Pred(子表的列上的条件,UNKNOWN 丢弃)、按 Order 排序,取 Take 的列,作为子行的列表加一列 As
// (类型 rows,值 Children)。没有子行 → 空列表(不是 NULL)。子行多于 N → 整个请求 FAILED_PRECONDITION
// (不截断;两个后端都最多取 N+1 行,超过由出口判定: 只看响应里的行)。
// DRC: Order + On 的子表的列覆盖子表的一个唯一键(不为空的列)→ 子行的顺序是全序。只有一层。
// 对方可以是逻辑表(Source): relation.go。
//
// SQL: LEFT JOIN LATERAL (
//
//	SELECT coalesce(jsonb_agg(jsonb_build_object(…) ORDER BY …), '[]') AS v
//	FROM (SELECT 用到的列 FROM 子表 WHERE … ORDER BY … LIMIT N+1) AS w
//
// ) ON TRUE(多取一行: 超过上限由出口判定;值按核心的 API 表示写进 JSON: jsonValueSQL)

import (
	"fmt"
	"strings"
)

// Nest: 一对多嵌套(子行的列表加一列 as)。
func Nest(table string, cols []NamedType, unique [][]string, on []JoinKey, pred *Expr, order []SortKey, max int, as string, take []TakeCol) Step {
	return Step{Kind: "nest", Table: table, Columns: cols, Unique: unique, On: on, Pred: pred, Order: order, N: max, As: as, Take: take}
}

func init() {
	registerStep(stepPrim{
		kind: "nest",
		check: func(c *checker, st Step, at string) {
			c.p.checkSource(st, at, c.add)
			c.cur = c.p.checkNest(c.cur, st, at, c.add)
		},
		eval: func(e *evaluator, st Step) error {
			child, err := e.p.related(st, e.tables, e.params)
			if err != nil {
				return err
			}
			e.schema, e.rows, err = e.p.evalNest(st, e.schema, e.rows, child, e.params)
			return err
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			lv, alias, err := g.lateral(lv, st, g.nestSQL)
			if err != nil {
				return nil, err
			}
			lv.refs[st.As] = alias + ".v"
			lv.in = g.p.checkNest(lv.in, st, "", func(string, ...any) {})
			lv.out = lv.in
			return lv, nil
		},
	})
}

// checkNest: 一对多嵌套的 DRC。子行的顺序必须是全序: Order 的列 + 关联的子表的列(同一个父行里是常量)
// 覆盖子表的一个唯一键,而且这个唯一键的列都不为空(可以为空的唯一列里 NULL 可以重复)。只有一层(子表的列不会是 rows)。
func (p *Pipeline) checkNest(cur Schema, st Step, at string, add func(string, ...any)) Schema {
	child, ok := p.checkChild(cur, st, at, add)
	if !ok {
		return cur
	}
	if st.N <= 0 {
		add("%s: 子行的上限 N 必须 > 0", at)
	}
	if st.As == "" {
		add("%s: 需要子行的列表的列名(as)", at)
	} else if _, dup := cur.Col(st.As); dup {
		add("%s: 列 %s 和已有的列重名", at, st.As)
	}
	if len(st.Order) == 0 {
		add("%s: 需要子行的顺序(order)", at)
	}
	checkOrder(child, st.Order, at, add)
	inOrder, inOn := map[string]bool{}, map[string]bool{}
	for _, o := range st.Order {
		inOrder[o.Col] = true
	}
	for _, k := range st.On {
		inOn[k.Key] = true
	}
	total := false
	srcKeys := SourceKeys(st.Source) // 逻辑表: 取最新的键(NULL 自己是一组)
	for _, u := range st.Unique {
		all := len(u) > 0
		nullsEq := sameSet(u, srcKeys)
		for _, c := range u {
			t, ok := child.Col(c)
			// 关联的列: 匹配到的子行里 = 父行的值(不为空的常量);顺序的列: 必须不为空
			// (逻辑表的取最新的键: 可以为空也行 —— 空值自己是一组,一个父行里最多一行键为空的子行)
			all = all && ok && (inOn[c] || (inOrder[c] && (!t.Nullable || nullsEq)))
		}
		total = total || all
	}
	if !total && len(st.Order) > 0 {
		add("%s: 子行的顺序 %v 不是全序: 顺序的列(加上关联的列)要包含 %s 的主键或唯一键(不在关联条件里的列要不为空)(唯一键: %v)", at, orderNames(st.Order), st.Table, st.Unique)
	}
	if len(st.Take) == 0 {
		add("%s: 需要子行的列(take)", at)
	}
	var fields Schema
	names := map[string]bool{}
	for _, t := range st.Take {
		ct, ok := child.Col(t.Col)
		switch {
		case !ok:
			add("%s: %s 没有列 %s", at, st.Table, t.Col)
			continue
		case ct.Kind == JSON || ct.Kind == Rows:
			add("%s: 子行的列 %s 不能是 %s", at, t.Name, ct.Kind)
			continue
		case names[t.Name]:
			add("%s: 子行的列 %s 重复", at, t.Name)
			continue
		}
		names[t.Name] = true
		fields = append(fields, NamedType{t.Name, ct})
	}
	return append(append(Schema(nil), cur...), NamedType{st.As, Type{Kind: Rows, Fields: fields}})
}

// evalNest: 子行按 Order 排序(并列而内容不同 → ErrAmbiguous),最多取 N+1 行。
func (p *Pipeline) evalNest(st Step, schema Schema, rows []Row, child []Row, params Params) (Schema, []Row, error) {
	childSchema := Schema(st.Columns)
	index, err := p.childIndex(st, child, params)
	if err != nil {
		return nil, nil, err
	}
	next := append(Schema(nil), schema...)
	var fields Schema
	for _, t := range st.Take {
		ct, _ := childSchema.Col(t.Col)
		fields = append(fields, NamedType{t.Name, ct})
	}
	next = append(next, NamedType{st.As, Type{Kind: Rows, Fields: fields}})
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		group := childGroup(r, st, index)
		row := Row{}
		for c, v := range r {
			row[c] = v
		}
		sortRows(group, st.Order)
		for i := 1; i < len(group); i++ {
			if compareRows(group[i-1], group[i], st.Order) == 0 && !sameRow(group[i-1], group[i]) {
				return nil, nil, fmt.Errorf("%w: 子行的顺序 %v 有并列的行", ErrAmbiguous, st.Order)
			}
		}
		if len(group) > st.N+1 {
			group = group[:st.N+1]
		}
		list := make([]Row, 0, len(group))
		for _, c := range group {
			item := Row{}
			for _, t := range st.Take {
				item[t.Name] = c[t.Col]
			}
			list = append(list, item)
		}
		if len(list) == 0 && st.Mutant == "empty_null" {
			row[st.As] = nil
		} else {
			row[st.As] = Children{Rows: list, Max: st.N}
		}
		out = append(out, row)
	}
	return next, out, nil
}

// nestSQL: 内层取用到的列(子行的列 + 顺序的列),外层按同样的顺序聚合成 JSON。
func (g *sqlGen) nestSQL(lv *sqlLevel, st Step) (string, error) {
	child, from, err := g.childFrom(lv, st)
	if err != nil {
		return "", err
	}
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
