package core

import "fmt"

// pick_per_key: Keys: 每个键(NULL 视为相等)按 Order 取第一行。并列而内容不同 → ErrAmbiguous。
// SQL: DISTINCT ON (键) … ORDER BY 键, Order(sql.go 的 render / distinctKeys)。筛选下推见 pushdown.go。

func PickPerKey(keys []string, order ...SortKey) Step {
	return Step{Kind: "pick_per_key", Keys: keys, Order: order}
}

func init() {
	registerStep(stepPrim{
		kind: "pick_per_key",
		check: func(c *checker, st Step, at string) {
			if len(st.Keys) == 0 || len(st.Order) == 0 {
				c.add("%s: 需要 keys 和 order", at)
			}
			for _, k := range st.Keys {
				t, ok := c.cur.Col(k)
				if !ok {
					c.add("%s: 键 %s 不存在", at, k)
				} else if t.Kind == JSON || t.Kind == Rows {
					c.add("%s: 键 %s 不能是 %s", at, k, t.Kind)
				}
				// 键可以为 NULL: NULL 自己是一组(和 SQL 的 DISTINCT ON 一样;参考求值器按规范文本分组,NULL = "∅")
			}
			checkOrder(c.cur, st.Order, at, c.add)
		},
		eval: func(e *evaluator, st Step) error {
			groups := map[string][]Row{}
			var order []string
			for _, r := range e.rows {
				k := keyString(r, st.Keys)
				if _, ok := groups[k]; !ok {
					order = append(order, k)
				}
				groups[k] = append(groups[k], r)
			}
			var out []Row
			for _, k := range order {
				g := groups[k]
				sortRows(g, st.Order)
				if len(g) > 1 && compareRows(g[0], g[1], st.Order) == 0 && !sameRow(g[0], g[1]) {
					return fmt.Errorf("%w: pick_per_key 的键 %s 有并列的行", ErrAmbiguous, k)
				}
				out = append(out, g[0])
			}
			e.rows = out
			return nil
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			lv.distinct, lv.dOrder = st.Keys, st.Order
			return lv, nil
		},
	})
}
