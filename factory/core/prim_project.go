package core

// project: Project: 新的列(名字 → 表达式),只保留这些列。

func Project(xs ...NamedExpr) Step { return Step{Kind: "project", Project: xs} }

func init() {
	registerStep(stepPrim{
		kind: "project",
		check: func(c *checker, st Step, at string) {
			var next Schema
			seen := map[string]bool{}
			for _, x := range st.Project {
				if seen[x.Name] {
					c.add("%s: 列 %s 重复", at, x.Name)
				}
				seen[x.Name] = true
				t, err := c.p.typeOf(x.Expr, c.cur)
				if err != nil {
					c.add("%s: 列 %s: %v", at, x.Name, err)
					continue
				}
				next = append(next, NamedType{x.Name, t})
			}
			for gi, k := range c.gkeys { // 分组键跟着改名(sort 的检查用)
				nk := ""
				for _, x := range st.Project {
					if k != "" && x.Expr.Op == "col" && x.Expr.Name == k {
						nk = x.Name
						break
					}
				}
				c.gkeys[gi] = nk
			}
			c.cur = next
		},
		eval: func(e *evaluator, st Step) error {
			var next Schema
			for _, x := range st.Project {
				t, err := e.p.typeOf(x.Expr, e.schema)
				if err != nil {
					return err
				}
				next = append(next, NamedType{x.Name, t})
			}
			var out []Row
			for _, r := range e.rows {
				row := Row{}
				for _, x := range st.Project {
					v, err := e.p.eval(x.Expr, r, e.schema, e.params)
					if err != nil {
						return err
					}
					row[x.Name] = v
				}
				out = append(out, row)
			}
			e.schema, e.rows = next, out
			return nil
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			lv.project = st.Project
			var s Schema
			for _, x := range st.Project {
				t, err := g.p.typeOf(x.Expr, lv.in)
				if err != nil {
					return nil, err
				}
				s = append(s, NamedType{x.Name, t})
			}
			lv.out = s
			return lv, nil
		},
	})
}
