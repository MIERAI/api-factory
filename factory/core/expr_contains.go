package core

// contains: Args[0] 包含参数 Name(任何位置;按字面,% _ \ 不是通配符;空字符串 → TRUE);任一边 NULL → UNKNOWN。
// 按字节比较(和 starts_with 一样,类型规则也一样: expr_starts_with.go 的 textMatchType)。
// btree 索引用不上(前面有通配符),大表要配 pg_trgm 的 GIN 索引。

import (
	"fmt"
	"strings"
)

func Contains(x *Expr, param string) *Expr {
	return &Expr{Op: "contains", Name: param, Args: []*Expr{x}}
}

func init() {
	registerExpr(exprPrim{
		op:      "contains",
		request: true,
		typeOf:  textMatchType,
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			a, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || a == nil || absent(params, e.Name) {
				return nil, err
			}
			return strings.Contains(a.(string), params[e.Name].(string)), nil
		},
		// 前后都是 %: 和 starts_with 同样的转义(按字面)。LIKE 而不是 strpos: pg_trgm 的 GIN 索引能用
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			a, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			p, _ := g.param(e.Name)
			return fmt.Sprintf(`(%s LIKE '%%' || replace(replace(replace(%s, '\', '\\'), '%%', '\%%'), '_', '\_') || '%%')`, a, p), nil
		},
		nullRejects: firstArgRejects, // 第一个参数 NULL → UNKNOWN
	})
}
