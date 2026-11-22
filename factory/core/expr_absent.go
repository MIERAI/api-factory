package core

// absent: Name = 参数名: 标量没有指定 / 列表为空或没有指定。"不传"只在这里定义(参考求值器: eval.go 的 absent)。
// 必填参数: 恒为 FALSE(SQL 里写 FALSE,不登记参数)。

import "fmt"

func Absent(param string) *Expr { return &Expr{Op: "absent", Name: param} }

func init() {
	registerExpr(exprPrim{
		op:      "absent",
		request: true,
		typeOf: func(p *Pipeline, e *Expr, _ Schema) (Type, error) {
			if p.Param(e.Name) == nil {
				return Type{}, fmt.Errorf("参数 %s 没有声明", e.Name)
			}
			return Type{Kind: Bool}, nil
		},
		eval: func(_ *Pipeline, e *Expr, _ Row, _ Schema, params Params) (any, error) {
			return absent(params, e.Name), nil
		},
		sql: func(g *sqlGen, e *Expr, _ *sqlLevel, _ bool) (string, error) {
			if g.p.Param(e.Name).Required { // 必填: 恒为 FALSE(不登记参数,否则 SQL 里有用不到的 $n)
				return "FALSE", nil
			}
			p, d := g.param(e.Name)
			switch {
			case d.List:
				return fmt.Sprintf("(coalesce(cardinality(%s), 0) = 0)", p), nil
			}
			return "(" + p + " IS NULL)", nil
		},
		nullRejects: func(p *Pipeline, e *Expr) (map[string]bool, bool) { // 必填参数: 恒为 FALSE
			if d := p.Param(e.Name); d != nil && d.Required {
				return map[string]bool{}, true
			}
			return map[string]bool{}, false
		},
	})
}
