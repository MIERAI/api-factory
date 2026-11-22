package core

// 逻辑:
//
//	and / or  Args(≥ 2),Kleene 三值逻辑(and: 有 FALSE 则 FALSE,否则有 UNKNOWN 则 UNKNOWN;or 对称)。
//	          SQL 的肯定位置穿过 and / or(keepPos)
//	not       Args[0](UNKNOWN → UNKNOWN)
//	is_null   Args[0];永远 TRUE / FALSE

import (
	"fmt"
	"strings"
)

func And(xs ...*Expr) *Expr { return &Expr{Op: "and", Args: xs} }
func Or(xs ...*Expr) *Expr  { return &Expr{Op: "or", Args: xs} }
func Not(x *Expr) *Expr     { return &Expr{Op: "not", Args: []*Expr{x}} }
func IsNull(x *Expr) *Expr  { return &Expr{Op: "is_null", Args: []*Expr{x}} }

func init() {
	for _, op := range []string{"and", "or"} {
		x := exprPrim{
			op:      op,
			keepPos: true,
			typeOf:  andOrType,
			eval:    andOrEval,
			sql:     andOrSQL,
		}
		if op == "and" {
			x.nullRejects = andRejects
		} else {
			x.nullRejects = orRejects
		}
		registerExpr(x)
	}
	registerExpr(exprPrim{
		op: "not",
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			t, err := p.typeOf(e.Args[0], cur)
			if err != nil {
				return Type{}, err
			}
			if t.Kind != Bool {
				return Type{}, fmt.Errorf("not 的参数必须是 bool")
			}
			return t, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			v, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || v == nil {
				return nil, err
			}
			return !v.(bool), nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			s, err := g.expr(e.Args[0], lv)
			return "(NOT " + s + ")", err
		},
		nullRejects: func(_ *Pipeline, e *Expr) (map[string]bool, bool) { // not(is_null(c))
			cols := map[string]bool{}
			if a := e.Args[0]; a.Op == "is_null" {
				rejectCol(cols, a.Args[0])
			}
			return cols, false
		},
	})
	registerExpr(exprPrim{
		op: "is_null",
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			if _, err := p.typeOf(e.Args[0], cur); err != nil {
				return Type{}, err
			}
			return Type{Kind: Bool}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			v, err := p.eval(e.Args[0], r, schema, params)
			if err != nil {
				return nil, err
			}
			return v == nil, nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			s, err := g.expr(e.Args[0], lv)
			return "(" + s + " IS NULL)", err
		},
	})
}

func andOrType(p *Pipeline, e *Expr, cur Schema) (Type, error) {
	if len(e.Args) < 2 {
		return Type{}, fmt.Errorf("%s 至少两个参数", e.Op)
	}
	nullable := false
	for _, x := range e.Args {
		t, err := p.typeOf(x, cur)
		if err != nil {
			return Type{}, err
		}
		if t.Kind != Bool {
			return Type{}, fmt.Errorf("%s 的参数必须是 bool", e.Op)
		}
		nullable = nullable || t.Nullable
	}
	return Type{Kind: Bool, Nullable: nullable}, nil
}

func andOrEval(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
	dominant := e.Op == "or" // or: TRUE 占优;and: FALSE 占优
	unknown := false
	for _, x := range e.Args {
		v, err := p.eval(x, r, schema, params)
		if err != nil {
			return nil, err
		}
		if v == nil {
			unknown = true
		} else if v.(bool) == dominant {
			return dominant, nil
		}
	}
	if unknown {
		return nil, nil
	}
	return !dominant, nil
}

func andOrSQL(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
	var xs []string
	for _, x := range e.Args {
		s, err := g.expr(x, lv)
		if err != nil {
			return "", err
		}
		if s == "FALSE" && e.Op == "or" { // 化简: 必填参数的 absent 恒为 FALSE
			continue
		}
		xs = append(xs, s)
	}
	switch len(xs) {
	case 0:
		return "FALSE", nil
	case 1:
		return xs[0], nil
	}
	return "(" + strings.Join(xs, " "+strings.ToUpper(e.Op)+" ") + ")", nil
}

// andRejects: 任何一项拒绝 → 整体拒绝。
func andRejects(p *Pipeline, e *Expr) (map[string]bool, bool) {
	cols := map[string]bool{}
	for _, a := range e.Args {
		c, al := nullRejects(a, p)
		if al {
			return cols, true
		}
		for k := range c {
			cols[k] = true
		}
	}
	return cols, false
}

// orRejects: 每一项都拒绝 → 整体拒绝。
func orRejects(p *Pipeline, e *Expr) (map[string]bool, bool) {
	cols := map[string]bool{}
	first := true
	for _, a := range e.Args {
		c, al := nullRejects(a, p)
		if al {
			continue
		}
		if first {
			cols, first = c, false
			continue
		}
		for k := range cols {
			if !c[k] {
				delete(cols, k)
			}
		}
	}
	if first { // 每一项都恒不为 TRUE
		return cols, true
	}
	return cols, false
}
