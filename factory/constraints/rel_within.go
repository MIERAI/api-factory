package constraints

// 不超过 N 天: "A 到 B 不超过 N 天"。两个都指定时 B - A ≤ N 天(时间 / 日期)。
// 日期在 API 里是 "YYYY-MM-DD" 的字符串: CEL 里按 UTC 0 点换成 timestamp 再相减。格式不对时这条规则不检查
// (字段自己的格式规则会报错;否则 timestamp() 的运行时错误会盖住真正的原因)。

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
			days, _ := strconv.Atoi(m[3])
			msg := fmt.Sprintf("%s to %s must be %d days or less", a.Name, b.Name, days)
			switch {
			case a.Kind == "time" && b.Kind == "time":
				return CEL{Args: []string{a.Name, b.Name, m[3]}, Message: msg,
					Expr: fmt.Sprintf("%sthis.%s - this.%s <= duration('%dh')", Guard(a, b), b.Name, a.Name, days*24)}, nil
			case a.Kind == "date" && b.Kind == "date":
				const re = `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`
				return CEL{Args: []string{a.Name, b.Name, m[3]}, Message: msg,
					Expr: fmt.Sprintf("%s!this.%s.matches('%s') || !this.%s.matches('%s') || timestamp(this.%s + 'T00:00:00Z') - timestamp(this.%s + 'T00:00:00Z') <= duration('%dh')",
						Guard(a, b), a.Name, re, b.Name, re, b.Name, a.Name, days*24)}, nil
			}
			return CEL{}, fmt.Errorf("不超过 N 天 只能用于两个时间或两个日期")
		},
		Cases: func(args []string, value func(string) any) []RelCase {
			a, b := args[0], args[1]
			days, _ := strconv.Atoi(args[2])
			av := value(a)
			n := time.Duration(days) * 24 * time.Hour
			over, overLabel := time.Second, "1 秒"
			if s, ok := av.(string); ok && len(s) == len("2006-01-02") { // 日期: 多 1 天
				over, overLabel = 24*time.Hour, "1 天"
			}
			return []RelCase{
				{Label: fmt.Sprintf("%s 到 %s 正好 %d 天(合法)", a, b, days), Set: map[string]any{a: av, b: shift(av, n)}, Valid: true},
				{Label: fmt.Sprintf("%s 到 %s 超过 %d 天 %s(违反)", a, b, days, overLabel), Set: map[string]any{a: av, b: shift(av, n+over)}},
			}
		},
	})
}
