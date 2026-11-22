package core

// in_tuples: 组的列表: Params 是同样长的列表参数(第 i 个组 = 每个参数的第 i 个值)。
// 有一个组和 Args(列)逐个相等 → TRUE,否则 FALSE(两值,和 EXISTS 一样: 列为 NULL 时不相等,不是 UNKNOWN);空列表 → FALSE。
// SQL: 并行的数组按下标对齐成组(单参数 unnest + WITH ORDINALITY)的 EXISTS。

import (
	"fmt"
	"strings"
)

func InTuples(cols []string, params []string) *Expr {
	e := &Expr{Op: "in_tuples", Params: params}
	for _, c := range cols {
		e.Args = append(e.Args, Col(c))
	}
	return e
}

func init() {
	registerExpr(exprPrim{
		op:      "in_tuples",
		request: true,
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			if len(e.Args) < 2 || len(e.Args) != len(e.Params) {
				return Type{}, fmt.Errorf("in_tuples: 至少两列,列和参数的个数必须相同")
			}
			for i, c := range e.Args {
				t, err := p.typeOf(c, cur)
				if err != nil {
					return Type{}, err
				}
				d := p.Param(e.Params[i])
				if d == nil || !d.List || !comparable(t.Kind, d.Type.Kind) {
					return Type{}, fmt.Errorf("in_tuples: 参数 %s 必须是 %s 的列表", e.Params[i], t.Kind)
				}
			}
			return Type{Kind: Bool}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			vals := make([]any, len(e.Args))
			for i, c := range e.Args {
				v, err := p.eval(c, r, schema, params)
				if err != nil {
					return nil, err
				}
				if v == nil {
					return false, nil
				}
				vals[i] = v
			}
			lists := make([][]any, len(e.Params))
			for i, n := range e.Params {
				lists[i], _ = params[n].([]any)
				if len(lists[i]) != len(lists[0]) {
					return nil, fmt.Errorf("in_tuples: 参数 %s 和 %s 的长度不同", n, e.Params[0])
				}
			}
		next:
			for k := range lists[0] {
				for i, v := range vals {
					if x := lists[i][k]; x == nil || compareValues(v, x) != 0 {
						continue next
					}
				}
				return true, nil
			}
			return false, nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) { // EXISTS 是两值的(和参考求值器一致)
			var from, conds []string
			for i, c := range e.Args {
				s, err := g.expr(c, lv)
				if err != nil {
					return "", err
				}
				p, _ := g.param(e.Params[i])
				from = append(from, fmt.Sprintf("unnest(%s) WITH ORDINALITY AS tup%d(v, n)", p, i+1))
				conds = append(conds, fmt.Sprintf("%s = tup%d.v", s, i+1))
				if i > 0 {
					conds = append(conds, fmt.Sprintf("tup%d.n = tup1.n", i+1))
				}
			}
			return fmt.Sprintf("EXISTS (SELECT 1 FROM %s WHERE %s)", strings.Join(from, ", "), strings.Join(conds, " AND ")), nil
		},
		nullRejects: func(_ *Pipeline, e *Expr) (map[string]bool, bool) { // 任一列 NULL → FALSE
			cols := map[string]bool{}
			for _, a := range e.Args {
				rejectCol(cols, a)
			}
			return cols, false
		},
	})
}
