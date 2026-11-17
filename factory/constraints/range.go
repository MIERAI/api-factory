package constraints

// 范围: 整数的上下限。设计语言 范围: "> 0" / ">= 1" / "0~100"(列表时用于每个元素)。

import (
	"errors"
	"fmt"
	"regexp"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"

	"apifactory/factory/ir"
)

var (
	reCmp     = regexp.MustCompile(`^\s*(>=|<=|>|<)\s*(-?\d+)\s*$`)
	reBetween = regexp.MustCompile(`^\s*(-?\d+)\s*~\s*(-?\d+)\s*$`)
)

func init() {
	register(&Constraint{
		Key: "范围", Level: Elem, Example: `"> 0", "0~100"`,
		Compile: func(c Ctx, v string) ([]string, error) {
			if c.ProtoType != "int32" && c.ProtoType != "int64" {
				return nil, errors.New("只能用于整数")
			}
			if m := reCmp.FindStringSubmatch(v); m != nil {
				op := map[string]string{">": "gt", ">=": "gte", "<": "lt", "<=": "lte"}[m[1]]
				return []string{fmt.Sprintf("%s.%s = %s", c.ProtoType, op, m[2])}, nil
			}
			if m := reBetween.FindStringSubmatch(v); m != nil {
				return []string{fmt.Sprintf("%s = {gte: %s, lte: %s}", c.ProtoType, m[1], m[2])}, nil
			}
			return nil, errors.New("无法识别")
		},
		Boundaries: func(r *validate.FieldRules, _ *ir.Field, _ any) ([]Boundary, string) {
			ir, typ := intRules(r)
			if ir == nil {
				return nil, ""
			}
			return intBoundaries(ir), typ + " 范围/in"
		},
		Domain: func(r *validate.FieldRules) *Domain {
			ir, _ := intRules(r)
			if ir == nil {
				return nil
			}
			var lo int64
			switch {
			case ir.gte != nil:
				lo = *ir.gte
			case ir.gt != nil:
				lo = *ir.gt + 1
			default:
				return nil
			}
			hi := lo + 10
			if ir.lte != nil {
				hi = *ir.lte
			} else if ir.lt != nil {
				hi = *ir.lt - 1
			}
			d := &Domain{Values: []any{lo, lo + 1, lo + 2, hi}}
			if ir.lte == nil && ir.lt == nil { // 没有上限: 可以无限扩大
				d.Expand = func(i int) any { return lo + int64(i) }
			}
			return d
		},
	})
}

type ints struct {
	gt, gte, lt, lte *int64
	in               []int64
}

func intRules(r *validate.FieldRules) (*ints, string) {
	p := func(v int64) *int64 { return &v }
	switch {
	case r.HasInt32():
		x := r.GetInt32()
		out := &ints{}
		if x.HasGt() {
			out.gt = p(int64(x.GetGt()))
		}
		if x.HasGte() {
			out.gte = p(int64(x.GetGte()))
		}
		if x.HasLt() {
			out.lt = p(int64(x.GetLt()))
		}
		if x.HasLte() {
			out.lte = p(int64(x.GetLte()))
		}
		for _, v := range x.GetIn() {
			out.in = append(out.in, int64(v))
		}
		return out, "int32"
	case r.HasInt64():
		x := r.GetInt64()
		out := &ints{in: x.GetIn()}
		if x.HasGt() {
			out.gt = p(x.GetGt())
		}
		if x.HasGte() {
			out.gte = p(x.GetGte())
		}
		if x.HasLt() {
			out.lt = p(x.GetLt())
		}
		if x.HasLte() {
			out.lte = p(x.GetLte())
		}
		return out, "int64"
	}
	return nil, ""
}

func intBoundaries(r *ints) []Boundary {
	var b []Boundary
	if r.gt != nil {
		b = append(b, Boundary{fmt.Sprintf("= %d(gt %d 的边界,违反)", *r.gt, *r.gt), *r.gt, false, false},
			Boundary{fmt.Sprintf("= %d(gt %d 的边界,合法)", *r.gt+1, *r.gt), *r.gt + 1, true, false})
	}
	if r.gte != nil {
		b = append(b, Boundary{fmt.Sprintf("= %d(gte %d 的边界,违反)", *r.gte-1, *r.gte), *r.gte - 1, false, false},
			Boundary{fmt.Sprintf("= %d(gte %d 的边界,合法)", *r.gte, *r.gte), *r.gte, true, false})
	}
	if r.lt != nil {
		b = append(b, Boundary{fmt.Sprintf("= %d(lt %d 的边界,违反)", *r.lt, *r.lt), *r.lt, false, false},
			Boundary{fmt.Sprintf("= %d(lt %d 的边界,合法)", *r.lt-1, *r.lt), *r.lt - 1, true, false})
	}
	if r.lte != nil {
		b = append(b, Boundary{fmt.Sprintf("= %d(lte %d 的边界,违反)", *r.lte+1, *r.lte), *r.lte + 1, false, false},
			Boundary{fmt.Sprintf("= %d(lte %d 的边界,合法)", *r.lte, *r.lte), *r.lte, true, false})
	}
	if len(r.in) > 0 {
		mx, mn := r.in[0], r.in[0]
		for _, v := range r.in {
			mx, mn = max(mx, v), min(mn, v)
		}
		b = append(b, Boundary{fmt.Sprintf("= %d(不在 in %v 内)", mx+1, r.in), mx + 1, false, false},
			Boundary{fmt.Sprintf("= %d(不在 in %v 内)", mn-1, r.in), mn - 1, false, false})
		for _, v := range r.in {
			b = append(b, Boundary{fmt.Sprintf("= %d(in %v 内)", v, r.in), v, true, false})
		}
	}
	return b
}
