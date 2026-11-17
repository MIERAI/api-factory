package constraints

// 类型的格式: 日期 / 小数 / 邮编 … 这些类型在 proto 里是带 pattern 的文本。
// 不是设计语言写的(类型自动带);边界值和值域来自类型的注册表(factory/kinds)。

import (
	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"

	"apifactory/factory/ir"
	"apifactory/factory/kinds"
)

func init() {
	register(&Constraint{
		Level: Elem,
		Boundaries: func(r *validate.FieldRules, _ *ir.Field, _ any) ([]Boundary, string) {
			t, ok := kinds.ByPattern(r.GetString().GetPattern())
			if !r.HasString() || !ok {
				return nil, ""
			}
			var b []Boundary
			for _, x := range t.Boundaries {
				b = append(b, Boundary{x.Label, x.Value, x.Valid, false})
			}
			return b, t.Name + "格式"
		},
		Domain: func(r *validate.FieldRules) *Domain {
			t, ok := kinds.ByPattern(r.GetString().GetPattern())
			if !r.HasString() || !ok || len(t.Domain) == 0 {
				return nil
			}
			return &Domain{Values: t.Domain, Missing: t.Missing}
		},
	})
}
