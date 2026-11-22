package core

// starts_with: Args[0] 以参数 Name 开头(按字面,% _ \ 不是通配符);任一边 NULL → UNKNOWN。按字节比较。
// SQL: x LIKE 转义(参数) || '%'(btree 索引: COLLATE "C" 的表达式索引能用)。

import (
	"fmt"
	"strings"
)

func StartsWith(x *Expr, param string) *Expr {
	return &Expr{Op: "starts_with", Name: param, Args: []*Expr{x}}
}

func init() {
	registerExpr(exprPrim{
		op:      "starts_with",
		request: true,
		typeOf:  textMatchType,
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			a, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || a == nil || absent(params, e.Name) {
				return nil, err
			}
			return strings.HasPrefix(a.(string), params[e.Name].(string)), nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			a, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			p, _ := g.param(e.Name)
			return fmt.Sprintf(`(%s LIKE replace(replace(replace(%s, '\', '\\'), '%%', '\%%'), '_', '\_') || '%%')`, a, p), nil
		},
		nullRejects: firstArgRejects, // 第一个参数 NULL → UNKNOWN
	})
}

// textMatchType: starts_with / contains 的类型: 文本列 + 文本的标量参数;可空 = 列可空或参数可以不传。
func textMatchType(p *Pipeline, e *Expr, cur Schema) (Type, error) {
	d, a, err := p.paramArgType(e, cur)
	if err != nil {
		return Type{}, err
	}
	if d.List || a.Kind != Text || d.Type.Kind != Text {
		return Type{}, fmt.Errorf("%s: 只能用于文本", e.Op)
	}
	return Type{Kind: Bool, Nullable: a.Nullable || !d.Required}, nil
}
