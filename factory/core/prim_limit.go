package core

import "fmt"

// limit: N;或者 Param = 参数名(必填的整数参数,例如分页的 row_limit)。之前必须有 sort(check.go 的 Check 末尾)。

func Limit(n int) Step         { return Step{Kind: "limit", N: n} }
func LimitParam(p string) Step { return Step{Kind: "limit", Param: p} }

func init() {
	registerStep(stepPrim{
		kind: "limit",
		check: func(c *checker, st Step, at string) {
			if st.Param != "" {
				d := c.p.Param(st.Param)
				if d == nil || d.List || !d.Required || (d.Type.Kind != Int32 && d.Type.Kind != Int64) {
					c.add("%s: 参数 %s 必须是必填的整数", at, st.Param)
				}
			} else if st.N <= 0 {
				c.add("%s: N 必须 > 0", at)
			}
		},
		eval: func(e *evaluator, st Step) error {
			n := st.N
			if st.Param != "" {
				v, _ := e.params[st.Param].(int64)
				n = int(v)
			}
			if n < 0 {
				return fmt.Errorf("limit: 负数 %d", n)
			}
			if len(e.rows) > n {
				e.rows = e.rows[:n]
			}
			return nil
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			if lv.limit != "" {
				lv = g.wrap(lv, false)
			}
			if st.Param != "" {
				lv.limit, _ = g.param(st.Param)
			} else {
				lv.limit = fmt.Sprint(st.N)
			}
			return lv, nil
		},
	})
}
