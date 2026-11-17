package constraints

// 早于: "A 早于 B"。两个都指定时 A < B(时间 / 日期 / 整数)。

import (
	"fmt"
	"regexp"
	"time"
)

func init() {
	registerRelation(&Relation{
		Name: "before", Pattern: regexp.MustCompile(`^(\w+)\s*早于\s*(\w+)$`), Example: "A 早于 B",
		Compile: func(field func(string) (RelField, error), m []string) (CEL, error) {
			a, err := field(m[1])
			if err != nil {
				return CEL{}, err
			}
			b, err := field(m[2])
			if err != nil {
				return CEL{}, err
			}
			return CEL{Args: []string{a.Name, b.Name}, Message: fmt.Sprintf("%s must be earlier than %s", a.Name, b.Name),
				Expr: fmt.Sprintf("%sthis.%s < this.%s", Guard(a, b), a.Name, b.Name)}, nil
		},
		Cases: func(args []string, value func(string) any) []RelCase {
			a, b := args[0], args[1]
			bv := value(b)
			step := time.Second
			if s, ok := bv.(string); ok && len(s) == len("2006-01-02") {
				step = 24 * time.Hour
			}
			return []RelCase{
				{Label: fmt.Sprintf("%s = %s(早于的边界,违反)", a, b), Set: map[string]any{a: bv, b: bv}},
				{Label: fmt.Sprintf("%s 晚于 %s(违反)", a, b), Set: map[string]any{a: shift(bv, step), b: bv}},
				{Label: fmt.Sprintf("%s 比 %s 早一点(早于的边界,合法)", a, b), Set: map[string]any{a: shift(bv, -step), b: bv}, Valid: true},
				{Label: fmt.Sprintf("只指定 %s(不检查,合法)", b), Set: map[string]any{b: bv}, Unset: []string{a}, Valid: true},
			}
		},
	})
}
