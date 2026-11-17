package constraints

// 个数: 列表的个数。设计语言 个数: "0~1000"。必填的列表至少 1 个(和 必填 合并)。

import (
	"errors"
	"fmt"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"

	"apifactory/factory/ir"
)

func init() {
	register(&Constraint{
		Key: "个数", Level: List, Example: `"0~1000"`,
		Compile: func(c Ctx, v string) ([]string, error) {
			m := reBetween.FindStringSubmatch(v)
			if m == nil {
				return nil, errors.New("无法识别")
			}
			lo := m[1]
			if c.Required && lo == "0" {
				lo = "1"
			}
			var rs []string
			if lo != "0" {
				rs = append(rs, "repeated.min_items = "+lo)
			}
			return append(rs, "repeated.max_items = "+m[2]), nil
		},
		Boundaries: func(r *validate.FieldRules, _ *ir.Field, elem any) ([]Boundary, string) {
			x := r.GetRepeated()
			rep := func(n uint64) []any {
				out := make([]any, n)
				for i := range out {
					out[i] = elem
				}
				return out
			}
			var b []Boundary
			var label string
			if x.HasMinItems() && x.GetMinItems() > 0 {
				n := x.GetMinItems()
				b = append(b, Boundary{fmt.Sprintf("%d 个(min_items %d 的边界,违反)", n-1, n), rep(n - 1), false, false},
					Boundary{fmt.Sprintf("%d 个(min_items %d 的边界,合法)", n, n), rep(n), true, false})
				label = "min_items"
			}
			if x.HasMaxItems() {
				n := x.GetMaxItems()
				b = append(b, Boundary{fmt.Sprintf("%d 个(max_items %d 的边界,违反)", n+1, n), rep(n + 1), false, false},
					Boundary{fmt.Sprintf("%d 个(max_items %d 的边界,合法)", n, n), rep(n), true, false})
				if label != "" {
					label += ", "
				}
				label += "max_items"
			}
			return b, label
		},
	})
}
