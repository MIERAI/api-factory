package core

import "fmt"

// sort: Order: 文本按字节;NULL 在 ASC 时最后、DESC 时最前。并列而内容不同 → ErrAmbiguous。
// group 之后: Order 必须包含全部分组键(全序)。sort 之后只能接 limit(check.go 的 Check 末尾)。

func Sort(order ...SortKey) Step { return Step{Kind: "sort", Order: order} }

func init() {
	registerStep(stepPrim{
		kind: "sort",
		check: func(c *checker, st Step, at string) {
			checkOrder(c.cur, st.Order, at, c.add)
			// 分组之后的行由分组键唯一确定: 排序必须包含全部分组键,顺序才是全序(前N条 / 分页都依赖它)
			if c.gkeys != nil {
				in := map[string]bool{}
				for _, o := range st.Order {
					in[o.Col] = true
				}
				for gi, k := range c.gkeys {
					if k == "" {
						c.add("%s: 分组键 #%d 被投影丢掉了: 分组之后的排序必须包含全部分组键", at, gi+1)
					} else if !in[k] {
						c.add("%s: 分组之后的排序必须包含全部分组键(缺 %s;否则不同的组可能并列,顺序不确定)", at, k)
					}
				}
			}
		},
		eval: func(e *evaluator, st Step) error {
			sortRows(e.rows, st.Order)
			for i := 1; i < len(e.rows); i++ {
				if compareRows(e.rows[i-1], e.rows[i], st.Order) == 0 && !sameRow(e.rows[i-1], e.rows[i]) {
					return fmt.Errorf("%w: 排序 %v 有并列的行", ErrAmbiguous, st.Order)
				}
			}
			e.ordered = true
			return nil
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			lv.order = st.Order
			return lv, nil
		},
	})
}
