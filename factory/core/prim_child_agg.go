package core

// child_agg: 按父行聚合: 对每一行,对关联的子行(同 nest 的 On / Pred)计算 Aggs(agg.go,集合可能为空:
// count 0、sum 0、min / max NULL),作为标量列加到当前行上。对方可以是逻辑表(Source): relation.go。
// SQL: LEFT JOIN LATERAL (SELECT 聚合(agg.go 的 aggSQL) AS 名字, … FROM 子表 WHERE …) ON TRUE
// (没有 GROUP BY: 永远一行,空集合也有)

import "strings"

// ChildAgg: 按父行聚合(每个聚合加一列)。
func ChildAgg(table string, cols []NamedType, on []JoinKey, pred *Expr, aggs ...Agg) Step {
	return Step{Kind: "child_agg", Table: table, Columns: cols, On: on, Pred: pred, Aggs: aggs}
}

func init() {
	registerStep(stepPrim{
		kind: "child_agg",
		check: func(c *checker, st Step, at string) {
			c.p.checkSource(st, at, c.add)
			c.cur = c.p.checkChildAgg(c.cur, st, at, c.add)
		},
		eval: func(e *evaluator, st Step) error {
			child, err := e.p.related(st, e.tables, e.params)
			if err != nil {
				return err
			}
			e.schema, e.rows, err = e.p.evalChildAgg(st, e.schema, e.rows, child, e.params)
			return err
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			lv, alias, err := g.lateral(lv, st, g.childAggSQL)
			if err != nil {
				return nil, err
			}
			for _, a := range st.Aggs {
				lv.refs[a.Name] = alias + "." + quoteIdent(a.Name)
			}
			lv.in = g.p.checkChildAgg(lv.in, st, "", func(string, ...any) {})
			lv.out = lv.in
			return lv, nil
		},
	})
}

// checkChildAgg: 按父行聚合的 DRC。聚合的类型来自 agg.go(集合可能为空: min / max 可空)。
func (p *Pipeline) checkChildAgg(cur Schema, st Step, at string, add func(string, ...any)) Schema {
	child, ok := p.checkChild(cur, st, at, add)
	if !ok {
		return cur
	}
	if len(st.Aggs) == 0 {
		add("%s: 需要聚合(aggs)", at)
	}
	next := append(Schema(nil), cur...)
	for _, a := range st.Aggs {
		if _, dup := next.Col(a.Name); dup || a.Name == "" {
			add("%s: 聚合的列名 %q 为空或和已有的列重名", at, a.Name)
			continue
		}
		var col *Type
		if a.Col != "" {
			t, ok := child.Col(a.Col)
			if !ok {
				add("%s: 聚合 %s: %s 没有列 %s", at, a.Name, st.Table, a.Col)
				continue
			}
			col = &t
		}
		t, err := AggType(a.Func, col, true)
		if err != nil {
			add("%s: 聚合 %s: %v", at, a.Name, err)
			continue
		}
		next = append(next, NamedType{a.Name, t})
	}
	return next
}

// evalChildAgg: 每个父行的子行上算每个聚合(agg.go 的 evalAgg)。
func (p *Pipeline) evalChildAgg(st Step, schema Schema, rows []Row, child []Row, params Params) (Schema, []Row, error) {
	childSchema := Schema(st.Columns)
	index, err := p.childIndex(st, child, params)
	if err != nil {
		return nil, nil, err
	}
	next := append(Schema(nil), schema...)
	for _, a := range st.Aggs {
		var col *Type
		if a.Col != "" {
			t, _ := childSchema.Col(a.Col)
			col = &t
		}
		t, err := AggType(a.Func, col, true)
		if err != nil {
			return nil, nil, err
		}
		next = append(next, NamedType{a.Name, t})
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		group := childGroup(r, st, index)
		row := Row{}
		for c, v := range r {
			row[c] = v
		}
		for _, a := range st.Aggs {
			var col Type
			vals := make([]any, len(group))
			if a.Col != "" {
				col, _ = childSchema.Col(a.Col)
				for i, c := range group {
					vals[i] = c[a.Col]
				}
			}
			v, err := evalAgg(a, vals, col)
			if err != nil {
				return nil, nil, err
			}
			switch {
			case len(group) == 0 && a.Func == "sum" && st.Mutant == "sum_empty_null":
				v = nil
			case len(group) == 0 && a.Func == "count" && st.Mutant == "count_outer":
				v = int64(1)
			}
			row[a.Name] = v
		}
		out = append(out, row)
	}
	return next, out, nil
}

// childAggSQL: SELECT 聚合 AS 名字, … FROM 子表 WHERE …
func (g *sqlGen) childAggSQL(lv *sqlLevel, st Step) (string, error) {
	child, from, err := g.childFrom(lv, st)
	if err != nil {
		return "", err
	}
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
