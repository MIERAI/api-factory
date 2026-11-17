package constraints

// 至少指定一个: "A、B 至少指定一个"(也可以写 "A 和 B" / "A, B")。都没指定时拒绝。

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var reSep = regexp.MustCompile(`\s*(?:、|,|，|和)\s*`)

func init() {
	registerRelation(&Relation{
		Name: "at_least_one", Pattern: regexp.MustCompile(`^(\w+(?:\s*(?:、|,|，|和)\s*\w+)+)\s*至少指定一个$`), Example: "A、B 至少指定一个",
		Compile: func(field func(string) (RelField, error), m []string) (CEL, error) {
			var names, present []string
			for _, n := range reSep.Split(strings.TrimSpace(m[1]), -1) {
				f, err := field(n)
				if err != nil {
					return CEL{}, err
				}
				if f.Required {
					return CEL{}, fmt.Errorf("%s 是必填的,这条规则没有意义", n)
				}
				if f.Absent() == "" {
					return CEL{}, fmt.Errorf("%s 不能区分\"没有指定\"(请用列表、时间,或者可以不指定的输入)", n)
				}
				names = append(names, f.Name)
				present = append(present, f.Present())
			}
			if len(names) < 2 {
				return CEL{}, errors.New("至少要两个输入")
			}
			return CEL{Args: names, Message: fmt.Sprintf("at least one of %s must be specified", strings.Join(names, ", ")),
				Expr: strings.Join(present, " || ")}, nil
		},
		Cases: func(args []string, value func(string) any) []RelCase {
			cs := []RelCase{{Label: fmt.Sprintf("%s 都没有指定(违反)", strings.Join(args, "、")), Set: map[string]any{}, Unset: args}}
			for i, a := range args {
				var others []string
				others = append(others, args[:i]...)
				others = append(others, args[i+1:]...)
				cs = append(cs, RelCase{Label: fmt.Sprintf("只指定 %s(合法)", a), Set: map[string]any{a: value(a)}, Unset: others, Valid: true})
			}
			return cs
		},
	})
}
