package core

// cmp: Name ∈ = <> < <= > >=;Args[0], Args[1]。任一边 NULL → UNKNOWN。文本按字节(大小比较加 COLLATE "C")。
// SQL: 肯定位置的 cmp(op, ymd(x), v) 改写成整数列上的比较(expr_ymd.go 的 ymdCmp)。

import "fmt"

func Cmp(op string, a, b *Expr) *Expr { return &Expr{Op: "cmp", Name: op, Args: []*Expr{a, b}} }

var cmpOps = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

func init() {
	registerExpr(exprPrim{
		op: "cmp",
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			if !cmpOps[e.Name] || len(e.Args) != 2 {
				return Type{}, fmt.Errorf("cmp: 运算符 %q / 参数个数不对", e.Name)
			}
			a, err := p.typeOf(e.Args[0], cur)
			if err != nil {
				return Type{}, err
			}
			b, err := p.typeOf(e.Args[1], cur)
			if err != nil {
				return Type{}, err
			}
			if !comparable(a.Kind, b.Kind) {
				return Type{}, fmt.Errorf("cmp: %s 和 %s 不能比较", a.Kind, b.Kind)
			}
			return Type{Kind: Bool, Nullable: a.Nullable || b.Nullable}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			a, err := p.eval(e.Args[0], r, schema, params)
			if err != nil {
				return nil, err
			}
			b, err := p.eval(e.Args[1], r, schema, params)
			if err != nil {
				return nil, err
			}
			if a == nil || b == nil {
				return nil, nil
			}
			c := compareValues(a, b)
			switch e.Name {
			case "=":
				return c == 0, nil
			case "<>":
				return c != 0, nil
			case "<":
				return c < 0, nil
			case "<=":
				return c <= 0, nil
			case ">":
				return c > 0, nil
			case ">=":
				return c >= 0, nil
			}
			return nil, fmt.Errorf("eval: unknown expr %q", e.Op)
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, pos bool) (string, error) {
			if pos && len(e.Args) == 2 && e.Args[0].Op == "ymd" {
				return g.ymdCmp(e, lv)
			}
			a, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			b, err := g.expr(e.Args[1], lv)
			if err != nil {
				return "", err
			}
			t, _ := g.p.typeOf(e.Args[0], lv.in)
			if t.Kind == Text && e.Name != "=" && e.Name != "<>" {
				a += ` COLLATE "C"`
			}
			return fmt.Sprintf("(%s %s %s)", a, e.Name, b), nil
		},
		nullRejects: func(_ *Pipeline, e *Expr) (map[string]bool, bool) { // 任一边 NULL → UNKNOWN
			cols := map[string]bool{}
			rejectCol(cols, e.Args[0])
			rejectCol(cols, e.Args[1])
			return cols, false
		},
	})
}
