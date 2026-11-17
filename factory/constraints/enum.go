package constraints

// 枚举: defined_only / not_in。不是设计语言写的(枚举类型自动带)。

import (
	"fmt"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"

	"apifactory/factory/ir"
)

func init() {
	register(&Constraint{
		Level: Elem,
		Boundaries: func(r *validate.FieldRules, f *ir.Field, _ any) ([]Boundary, string) {
			if !r.HasEnum() {
				return nil, ""
			}
			x := r.GetEnum()
			var b []Boundary
			bad := map[int32]bool{}
			for _, v := range x.GetNotIn() {
				bad[v] = true
				b = append(b, Boundary{fmt.Sprintf("= %d(not_in)", v), int64(v), false, false})
			}
			if x.GetDefinedOnly() {
				mx := int32(0)
				for _, v := range f.Values {
					mx = max(mx, v.Number)
				}
				b = append(b, Boundary{fmt.Sprintf("= %d(未定义的值,defined_only)", mx+1), int64(mx + 1), false, false})
			}
			for _, v := range f.Values {
				if !bad[v.Number] {
					b = append(b, Boundary{fmt.Sprintf("= %s", v.Name), v.Name, true, false})
				}
			}
			return b, "enum defined_only/not_in"
		},
	})
}
