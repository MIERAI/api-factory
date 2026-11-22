package core

// rank: Order + As: 按 Order 的名次(SQL 的 RANK(): 并列同名次,下一个名次跳过),加一列 As(int64)。
// 只能在 group 之后。文本按字节;NULL 的位置和 sort 一样。
// SQL: 总是包一层,rank() OVER (ORDER BY …)(sql.go 的 render)。

func Rank(as string, order ...SortKey) Step { return Step{Kind: "rank", As: as, Order: order} }

func init() {
	registerStep(stepPrim{
		kind: "rank",
		check: func(c *checker, st Step, at string) {
			if !c.grouped {
				c.add("%s: rank 只能放在 group 之后", at)
			}
			if len(st.Order) == 0 {
				c.add("%s: 需要 order", at)
			}
			checkOrder(c.cur, st.Order, at, c.add)
			if st.As == "" {
				c.add("%s: 需要名次的列名(as)", at)
			} else if _, dup := c.cur.Col(st.As); dup {
				c.add("%s: 名次的列名 %s 和已有的列重名", at, st.As)
			}
			c.cur = append(append(Schema(nil), c.cur...), NamedType{st.As, Type{Kind: Int64}})
		},
		eval: func(e *evaluator, st Step) error {
			e.rows = evalRank(e.rows, st)
			e.schema = append(append(Schema(nil), e.schema...), NamedType{st.As, Type{Kind: Int64}})
			return nil
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) { // 窗口函数的 ORDER BY 引用 group 的输出列
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			r := st
			lv.rank = &r
			lv.out = append(append(Schema(nil), lv.in...), NamedType{st.As, Type{Kind: Int64}})
			return lv, nil
		},
	})
}

// evalRank: 名次 = 1 + 排在它前面(按 Order 严格小于)的行数。并列同名次,下一个名次跳过(SQL 的 RANK())。
// 行的顺序不变(rank 之后仍然是无序的,顺序由后面的 sort 决定)。
func evalRank(rows []Row, st Step) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		n := int64(1)
		for _, o := range rows {
			if compareRows(o, r, st.Order) < 0 {
				n++
			}
		}
		row := Row{}
		for k, v := range r {
			row[k] = v
		}
		row[st.As] = n
		out[i] = row
	}
	return out
}
