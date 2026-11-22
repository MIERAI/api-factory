package core

// scan: 流水线的第一步。Table;Columns = 物理表的列(名字 → 类型)。

func Scan(table string, cols ...NamedType) Step {
	return Step{Kind: "scan", Table: table, Columns: cols}
}

func init() {
	registerStep(stepPrim{
		kind: "scan",
		check: func(c *checker, st Step, at string) {
			if c.i != 0 {
				c.add("%s: scan 只能在第一步", at)
			}
			c.cur = append(Schema(nil), st.Columns...)
		},
		eval: func(e *evaluator, st Step) error {
			e.schema = append(Schema(nil), st.Columns...)
			for _, r := range e.tables[st.Table] {
				row := Row{}
				for _, c := range st.Columns {
					row[c.Name] = r[c.Name]
				}
				e.rows = append(e.rows, row)
			}
			return nil
		},
		sql: func(g *sqlGen, _ *sqlLevel, st Step) (*sqlLevel, error) {
			return g.newLevel(quoteIdent(st.Table), Schema(st.Columns)), nil
		},
	})
}
