package constraints

// 不超过 N 天: "A 到 B 不超过 N 天"。两个都指定时 B - A ≤ N 天(时间)。

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

func init() {
	registerRelation(&Relation{
		Name: "within", Pattern: regexp.MustCompile(`^(\w+)\s*到\s*(\w+)\s*不超过\s*(\d+)\s*天$`), Example: "A 到 B 不超过 N 天",
		Compile: func(field func(string) (RelField, error), m []string) (CEL, error) {
			a, err := field(m[1])
			if err != nil {
				return CEL{}, err
			}
			b, err := field(m[2])
			if err != nil {
				return CEL{}, err
			}
			if a.Kind != "time" || b.Kind != "time" {
				return CEL{}, fmt.Errorf("不超过 N 天 只能用于时间")
			}
			days, _ := strconv.Atoi(m[3])
			return CEL{Args: []string{a.Name, b.Name, m[3]}, Message: fmt.Sprintf("%s to %s must be %d days or less", a.Name, b.Name, days),
				Expr: fmt.Sprintf("%sthis.%s - this.%s <= duration('%dh')", Guard(a, b), b.Name, a.Name, days*24)}, nil
		},
		Cases: func(args []string, value func(string) any) []RelCase {
			a, b := args[0], args[1]
			days, _ := strconv.Atoi(args[2])
			av := value(a)
			n := time.Duration(days) * 24 * time.Hour
			return []RelCase{
				{Label: fmt.Sprintf("%s 到 %s 正好 %d 天(合法)", a, b, days), Set: map[string]any{a: av, b: shift(av, n)}, Valid: true},
				{Label: fmt.Sprintf("%s 到 %s 超过 %d 天 1 秒(违反)", a, b, days), Set: map[string]any{a: av, b: shift(av, n+time.Second)}},
			}
		},
	})
}
