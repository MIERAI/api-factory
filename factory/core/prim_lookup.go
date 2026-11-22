package core

// lookup: 多对一查找: 对每一行,在 Table(列 = Columns)里找 On 的键列逐个等于当前行的列的那一行,
// 把 Take 的列加到当前行上。On 的键列必须覆盖 Table 的一个唯一键(Unique)→ 最多一行匹配。
// 任一边为 NULL → 不匹配(和 SQL 的 = 一致)。找不到: Missing = keep(默认)→ 加的列为 NULL(类型可空);
// drop → 丢弃这一行(内连接)。后面的 lookup 可以用前面 lookup 加的列做关联(链式)。
// 对方可以是逻辑表(Source): relation.go。
// SQL: LEFT JOIN(drop: JOIN)接在这一层的 FROM 后面;逻辑表: LEFT JOIN LATERAL (来源 + 关联条件) ON TRUE。

import (
	"fmt"
	"strings"
)

// Lookup: 多对一查找(missing: keep / drop)。
func Lookup(table string, cols []NamedType, unique [][]string, on []JoinKey, take []TakeCol, missing string) Step {
	return Step{Kind: "lookup", Table: table, Columns: cols, Unique: unique, On: on, Take: take, Missing: missing}
}

func init() {
	registerStep(stepPrim{
		kind: "lookup",
		check: func(c *checker, st Step, at string) {
			c.p.checkSource(st, at, c.add)
			extra := c.p.checkLookupExt(c.cur, st, at, c.add)
			c.cur = checkLookupWith(c.cur, st, at, c.add, extra)
		},
		eval: func(e *evaluator, st Step) error {
			other, err := e.p.related(st, e.tables, e.params)
			if err != nil {
				return err
			}
			if st.Pred != nil || st.AsOf != nil {
				e.schema, e.rows, err = e.p.evalLookupExt(st, e.schema, e.rows, other, e.params)
				return err
			}
			e.schema, e.rows = evalLookup(st, e.schema, e.rows, other)
			return nil
		},
		sql: lookupSQL,
	})
}

// lookupSQL: WHERE 只引用之前的列,和连接的先后无关,不用包。
func lookupSQL(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
	if lv.joinBusy() {
		lv = g.wrap(lv, false)
	}
	if (st.Pred != nil || st.AsOf != nil) && len(st.Source) == 0 { // 按日期 / 带对方的条件: LATERAL(lookup_asof.go)
		lv, err := g.lookupLateralSQL(lv, st)
		if err != nil {
			return nil, err
		}
		lv.in = checkLookup(lv.in, st, "", func(string, ...any) {})
		lv.out = lv.in
		return lv, nil
	}
	if len(st.Source) > 0 { // 逻辑表: LEFT JOIN LATERAL (来源 + 关联条件) ON TRUE(drop: JOIN LATERAL)
		sub, err := g.sourceSQL(st, lv)
		if err != nil {
			return nil, err
		}
		g.n++
		alias := fmt.Sprintf("t%d", g.n)
		join := "LEFT JOIN LATERAL"
		if st.Missing == "drop" {
			join = "JOIN LATERAL"
		}
		lv.from += fmt.Sprintf("\n%s (\n%s\n) AS %s ON TRUE", join, indent(sub), alias)
		if lv.refs == nil {
			lv.refs = map[string]string{}
		}
		for _, t := range st.Take {
			lv.refs[t.Name] = alias + "." + quoteIdent(t.Col)
		}
		lv.in = checkLookup(lv.in, st, "", func(string, ...any) {})
		lv.out = lv.in
		return lv, nil
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
	return lv, nil
}

// checkLookup: 多对一查找的 DRC。对方的键列必须覆盖对方的一个唯一键(最多一行匹配;多对一以外的关联不是 lookup),
// 两边的类型可以比较,加的列不能和已有的列重名。返回加了列之后的列(keep: 加的列可空)。
func checkLookup(cur Schema, st Step, at string, add func(string, ...any)) Schema {
	return checkLookupWith(cur, st, at, add, pinnedAndStart(st))
}

// pinnedAndStart: 唯一性用的另外覆盖的列(对方的条件固定的列 + 按日期的开始列;lookup_asof.go)。
func pinnedAndStart(st Step) map[string]bool {
	extra := PinnedCols(st.Pred)
	if st.AsOf != nil {
		extra[st.AsOf.Start] = true
	}
	return extra
}

// checkLookupWith: checkLookup 的本体。extra: 键以外覆盖唯一键的列(固定的列 / 开始列)。
func checkLookupWith(cur Schema, st Step, at string, add func(string, ...any), extra map[string]bool) Schema {
	other := Schema(st.Columns)
	if st.Table == "" || len(st.Columns) == 0 {
		add("%s: 需要对方的表和列", at)
		return cur
	}
	if len(st.On) == 0 || len(st.Take) == 0 {
		add("%s: 需要关联条件(on)和加的列(take)", at)
	}
	if st.Missing != "" && st.Missing != "keep" && st.Missing != "drop" {
		add("%s: 找不到时只能是 keep / drop,实际是 %q", at, st.Missing)
	}
	keys := map[string]bool{}
	for _, k := range st.On {
		a, okA := cur.Col(k.Col)
		b, okB := other.Col(k.Key)
		switch {
		case !okA:
			add("%s: 关联的列 %s 不存在", at, k.Col)
		case !okB:
			add("%s: %s 没有列 %s", at, st.Table, k.Key)
		case !comparable(a.Kind, b.Kind):
			add("%s: 关联条件 %s = %s.%s: %s 和 %s 不能比较", at, k.Col, st.Table, k.Key, a.Kind, b.Kind)
		}
		if keys[k.Key] {
			add("%s: 对方的键列 %s 重复", at, k.Key)
		}
		keys[k.Key] = true
	}
	unique := false
	for _, u := range st.Unique {
		covered := len(u) > 0
		for _, c := range u {
			covered = covered && (keys[c] || extra[c])
		}
		unique = unique || covered
	}
	if !unique && len(st.On) > 0 {
		var ks []string
		for _, k := range st.On {
			ks = append(ks, k.Key)
		}
		add("%s: 关联的键 %v 不是 %s 的主键或唯一约束(唯一键: %v)。多对一以外的关联不能用查找", at, ks, st.Table, st.Unique)
	}
	next := append(Schema(nil), cur...)
	seen := map[string]bool{}
	for _, c := range cur {
		seen[c.Name] = true
	}
	for _, t := range st.Take {
		ct, ok := other.Col(t.Col)
		if !ok {
			add("%s: %s 没有列 %s", at, st.Table, t.Col)
			continue
		}
		if seen[t.Name] {
			add("%s: 加的列 %s 和已有的列重名", at, t.Name)
			continue
		}
		seen[t.Name] = true
		if st.Missing != "drop" { // 找不到 → NULL
			ct.Nullable = true
		}
		next = append(next, NamedType{t.Name, ct})
	}
	return next
}

// evalLookup: 多对一查找。对方的行按键建索引(键里有 NULL 的行不进索引: NULL 和什么都不相等),
// 当前行的关联列有 NULL → 找不到。匹配的行按连接的语义逐个输出(DRC 保证最多一行;
// 差分测试的"错误实现"(少比较一列的键)会匹配到多行,这时和 SQL 的连接一样输出多行)。
func evalLookup(st Step, schema Schema, rows []Row, other []Row) (Schema, []Row) {
	types := map[string]Type{}
	for _, c := range st.Columns {
		types[c.Name] = c.Type
	}
	next := append(Schema(nil), schema...)
	for _, t := range st.Take {
		ty := types[t.Col]
		if st.Missing != "drop" {
			ty.Nullable = true
		}
		next = append(next, NamedType{t.Name, ty})
	}
	const null = "\x01NULL" // NullEq(错误实现): NULL 当作一个普通的值
	key := func(r Row, cols func(JoinKey) string) (string, bool) {
		var b strings.Builder
		for _, k := range st.On {
			v := r[cols(k)]
			if v == nil {
				if !st.NullEq {
					return "", false
				}
				b.WriteString(null)
			} else {
				b.WriteString(Canonical(v))
			}
			b.WriteString("\x00")
		}
		return b.String(), true
	}
	index := map[string][]Row{}
	for _, o := range other {
		if k, ok := key(o, func(j JoinKey) string { return j.Key }); ok {
			index[k] = append(index[k], o)
		}
	}
	var out []Row
	for _, r := range rows {
		var found []Row
		if k, ok := key(r, func(j JoinKey) string { return j.Col }); ok {
			found = index[k]
		}
		if len(found) == 0 {
			if st.Missing == "drop" {
				continue
			}
			found = []Row{nil}
		}
		for _, o := range found {
			row := Row{}
			for c, v := range r {
				row[c] = v
			}
			for _, t := range st.Take {
				if o == nil {
					row[t.Name] = nil
				} else {
					row[t.Name] = o[t.Col]
				}
			}
			out = append(out, row)
		}
	}
	return next, out
}

// evalLookupExt: 带对方的条件 / 按日期的 lookup(lookup_asof.go)。对方的行先过 Pred,按键建索引;
// 按日期: 匹配的行里选一行;否则和 evalLookup 一样(DRC 保证最多一行)。
func (p *Pipeline) evalLookupExt(st Step, schema Schema, rows []Row, other []Row, params Params) (Schema, []Row, error) {
	otherSchema := Schema(st.Columns)
	var kept []Row
	for _, o := range other {
		if st.Pred != nil {
			v, err := p.eval(st.Pred, o, otherSchema, params)
			if err != nil {
				return nil, nil, err
			}
			if v != true {
				continue
			}
		}
		kept = append(kept, o)
	}
	if st.AsOf == nil {
		plain := st
		plain.Pred = nil
		next, out := evalLookup(plain, schema, rows, kept)
		return next, out, nil
	}
	index := map[string][]Row{}
	for _, o := range kept {
		if k, ok := childKey(o, st.On, func(j JoinKey) string { return j.Key }); ok {
			index[k] = append(index[k], o)
		}
	}
	next := append(Schema(nil), schema...)
	for _, t := range st.Take {
		ty, _ := otherSchema.Col(t.Col)
		if st.Missing != "drop" {
			ty.Nullable = true
		}
		next = append(next, NamedType{t.Name, ty})
	}
	var out []Row
	for _, r := range rows {
		var found []Row
		if k, ok := childKey(r, st.On, func(j JoinKey) string { return j.Col }); ok {
			found = index[k]
		}
		best, err := p.asOfPick(st, r, schema, params, found)
		if err != nil {
			return nil, nil, err
		}
		if best == nil && st.Missing == "drop" {
			continue
		}
		row := Row{}
		for c, v := range r {
			row[c] = v
		}
		for _, t := range st.Take {
			if best == nil {
				row[t.Name] = nil
			} else {
				row[t.Name] = best[t.Col]
			}
		}
		out = append(out, row)
	}
	return next, out, nil
}
