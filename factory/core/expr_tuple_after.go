package core

// tuple_after: Args(列)按 Desc 方向严格在参数 Params 之后(keyset 游标)。参数 Params[0] 没有指定 → TRUE。
// 游标列必须 NOT NULL。SQL: 行比较 (列…) > (参数…)(索引能用;文本 COLLATE "C")。

import (
	"fmt"
	"strings"
)

func TupleAfter(cols []string, params []string, desc bool) *Expr {
	e := &Expr{Op: "tuple_after", Params: params, Desc: desc}
	for _, c := range cols {
		e.Args = append(e.Args, Col(c))
	}
	return e
}

func init() {
	registerExpr(exprPrim{
		op:      "tuple_after",
		request: true,
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			if len(e.Args) == 0 || len(e.Args) != len(e.Params) {
				return Type{}, fmt.Errorf("tuple_after: 列和参数的个数必须相同")
			}
			for i, c := range e.Args {
				t, err := p.typeOf(c, cur)
				if err != nil {
					return Type{}, err
				}
				if t.Nullable {
					return Type{}, fmt.Errorf("tuple_after: 游标列 %s 必须 NOT NULL", c.Name)
				}
				d := p.Param(e.Params[i])
				if d == nil || d.List || !comparable(t.Kind, d.Type.Kind) {
					return Type{}, fmt.Errorf("tuple_after: 参数 %s 的类型不对", e.Params[i])
				}
			}
			return Type{Kind: Bool}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			if absent(params, e.Params[0]) {
				return true, nil
			}
			for i, c := range e.Args {
				v, err := p.eval(c, r, schema, params)
				if err != nil {
					return nil, err
				}
				cmp := compareValues(v, params[e.Params[i]])
				if e.Desc {
					cmp = -cmp
				}
				if cmp != 0 {
					return cmp > 0, nil
				}
			}
			return false, nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			var cols, ps []string
			var first string
			for i, c := range e.Args {
				s, err := g.expr(c, lv)
				if err != nil {
					return "", err
				}
				if t, _ := g.p.typeOf(c, lv.in); t.Kind == Text {
					s += ` COLLATE "C"`
				}
				cols = append(cols, s)
				p, _ := g.param(e.Params[i])
				if i == 0 {
					first = p
				}
				ps = append(ps, p)
			}
			op := ">"
			if e.Desc {
				op = "<"
			}
			return fmt.Sprintf("(%s IS NULL OR (%s) %s (%s))", first, strings.Join(cols, ", "), op, strings.Join(ps, ", ")), nil
		},
	})
}
