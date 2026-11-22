package core

// in_list: Args[0] 属于参数 Name 的列表。Args[0] 为 NULL → UNKNOWN;空列表 → FALSE。SQL: x = ANY(参数)。

import "fmt"

func InList(x *Expr, param string) *Expr { return &Expr{Op: "in_list", Name: param, Args: []*Expr{x}} }

func init() {
	registerExpr(exprPrim{
		op:      "in_list",
		request: true,
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			d, a, err := p.paramArgType(e, cur)
			if err != nil {
				return Type{}, err
			}
			if !d.List || !comparable(a.Kind, d.Type.Kind) {
				return Type{}, fmt.Errorf("in_list: 参数 %s 必须是 %s 的列表", e.Name, a.Kind)
			}
			return Type{Kind: Bool, Nullable: a.Nullable}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			a, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || a == nil {
				return nil, err
			}
			list, _ := params[e.Name].([]any)
			for _, x := range list {
				if compareValues(a, x) == 0 {
					return true, nil
				}
			}
			return false, nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			a, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			p, _ := g.param(e.Name)
			return fmt.Sprintf("(%s = ANY(%s))", a, p), nil
		},
		nullRejects: firstArgRejects, // 第一个参数 NULL → UNKNOWN
	})
}
