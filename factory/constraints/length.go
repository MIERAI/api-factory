package constraints

// 长度: 文本的长度(字符数)。设计语言 长度: "1~64"(列表时用于每个元素)。

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"

	"apifactory/factory/ir"
)

func init() {
	register(&Constraint{
		Key: "长度", Level: Elem, Example: `"1~64"`,
		Compile: func(c Ctx, v string) ([]string, error) {
			if c.ProtoType != "string" {
				return nil, errors.New("只能用于文本")
			}
			m := reBetween.FindStringSubmatch(v)
			if m == nil {
				return nil, errors.New("无法识别")
			}
			// 逐个字段写(不写 string = {…}): 和 格式 的 string.pattern 一起用时不会重复设置 string
			return []string{"string.min_len = " + m[1], "string.max_len = " + m[2]}, nil
		},
		Boundaries: func(r *validate.FieldRules, _ *ir.Field, _ any) ([]Boundary, string) {
			if !r.HasString() {
				return nil, ""
			}
			x := r.GetString()
			// 合法的边界值也要满足同一字段的格式(格式: 数字 时用 "0" 填充,不用 "a")
			fill := "a"
			if p := x.GetPattern(); p != "" {
				if re, err := regexp.Compile(p); err == nil {
					for _, c := range []string{"a", "0", "A", "1"} {
						if re.MatchString(c) && re.MatchString(c+c) {
							fill = c
							break
						}
					}
				}
			}
			var b []Boundary
			if x.HasMinLen() && x.GetMinLen() > 0 {
				n := int(x.GetMinLen())
				b = append(b, Boundary{fmt.Sprintf("长度 %d(min_len %d 的边界,违反)", n-1, n), strings.Repeat(fill, n-1), false, false},
					Boundary{fmt.Sprintf("长度 %d(min_len %d 的边界,合法)", n, n), strings.Repeat(fill, n), true, false})
			}
			if x.HasMaxLen() {
				n := int(x.GetMaxLen())
				b = append(b, Boundary{fmt.Sprintf("长度 %d(max_len %d 的边界,违反)", n+1, n), strings.Repeat(fill, n+1), false, false},
					Boundary{fmt.Sprintf("长度 %d(max_len %d 的边界,合法)", n, n), strings.Repeat(fill, n), true, false})
			}
			if len(b) == 0 {
				return nil, ""
			}
			return b, "string 长度"
		},
	})
}
