package core

// filter: Pred: 只保留 TRUE 的行(UNKNOWN 丢弃)。之后条件拒绝 NULL 的列收窄成 NOT NULL(check.go 的 narrow)。

func Filter(p *Expr) Step { return Step{Kind: "filter", Pred: p} }

func init() {
	registerStep(stepPrim{
		kind: "filter",
		check: func(c *checker, st Step, at string) {
			t, err := c.p.typeOf(st.Pred, c.cur)
			if err != nil {
				c.add("%s: %v", at, err)
			} else if t.Kind != Bool {
				c.add("%s: 条件必须是 bool,实际是 %s", at, t.Kind)
			}
			c.cur = narrow(c.cur, st.Pred, c.p)
		},
		eval: func(e *evaluator, st Step) error {
			var out []Row
			for _, r := range e.rows {
				v, err := e.p.eval(st.Pred, r, e.schema, e.params)
				if err != nil {
					return err
				}
				if v == true {
					out = append(out, r)
				}
			}
			e.rows = out
			return nil
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			w, err := g.where(st.Pred, lv)
			if err != nil {
				return nil, err
			}
			lv.where = append(lv.where, w)
			return lv, nil
		},
	})
}
