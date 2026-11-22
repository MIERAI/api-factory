package core

// 值的引用:
//
//	col      Name = 列名
//	const    Value(Type 决定种类;nil = NULL)
//	param    Name = 参数名(没有指定 → NULL;列表参数只能用在 in_list / absent / in_tuples)
//	sql_ref  SQL 后端内部: 外层查询的列(LATERAL 子查询里的关联条件,Name = SQL 的原样文本)。流水线里不会出现

import "fmt"

func Col(name string) *Expr     { return &Expr{Op: "col", Name: name} }
func Param(name string) *Expr   { return &Expr{Op: "param", Name: name} }
func Const(v any, t Type) *Expr { return &Expr{Op: "const", Value: v, Type: &t} }

func init() {
	registerExpr(exprPrim{
		op: "col",
		typeOf: func(_ *Pipeline, e *Expr, cur Schema) (Type, error) {
			t, ok := cur.Col(e.Name)
			if !ok {
				return Type{}, fmt.Errorf("列 %s 不存在", e.Name)
			}
			return t, nil
		},
		eval: func(_ *Pipeline, e *Expr, r Row, _ Schema, _ Params) (any, error) {
			return r[e.Name], nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			return g.col(lv, e.Name), nil
		},
	})
	registerExpr(exprPrim{
		op: "const",
		typeOf: func(_ *Pipeline, e *Expr, _ Schema) (Type, error) {
			if e.Type == nil {
				return Type{}, fmt.Errorf("const 需要类型")
			}
			t := *e.Type
			t.Nullable = e.Value == nil
			return t, nil
		},
		eval: func(_ *Pipeline, e *Expr, _ Row, _ Schema, _ Params) (any, error) {
			return e.Value, nil
		},
		sql: func(_ *sqlGen, e *Expr, _ *sqlLevel, _ bool) (string, error) {
			return constSQL(e.Value, e.Type.Kind)
		},
		nullRejects: func(_ *Pipeline, e *Expr) (map[string]bool, bool) { // FALSE: 恒不为 TRUE
			if b, ok := e.Value.(bool); ok && !b {
				return map[string]bool{}, true
			}
			return map[string]bool{}, false
		},
	})
	registerExpr(exprPrim{
		op:      "param",
		request: true,
		typeOf: func(p *Pipeline, e *Expr, _ Schema) (Type, error) {
			d := p.Param(e.Name)
			if d == nil {
				return Type{}, fmt.Errorf("参数 %s 没有声明", e.Name)
			}
			if d.List {
				return Type{}, fmt.Errorf("列表参数 %s 只能用在 in_list / absent", e.Name)
			}
			t := d.Type
			t.Nullable = !d.Required
			return t, nil
		},
		eval: func(_ *Pipeline, e *Expr, _ Row, _ Schema, params Params) (any, error) {
			if absent(params, e.Name) {
				return nil, nil
			}
			return params[e.Name], nil
		},
		sql: func(g *sqlGen, e *Expr, _ *sqlLevel, _ bool) (string, error) {
			s, _ := g.param(e.Name)
			return s, nil
		},
	})
	registerExpr(exprPrim{
		op: "sql_ref",
		typeOf: func(_ *Pipeline, e *Expr, _ Schema) (Type, error) {
			if e.Type == nil {
				return Type{}, fmt.Errorf("sql_ref 需要类型")
			}
			return *e.Type, nil
		},
		sql: func(_ *sqlGen, e *Expr, _ *sqlLevel, _ bool) (string, error) {
			return e.Name, nil
		},
	})
}
