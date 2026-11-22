package constraints

// 个数: 列表的个数。设计语言 个数: "0~1000"。必填的列表至少 1 个(和 必填 合并)。

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

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
		Boundaries: func(r *validate.FieldRules, f *ir.Field, elem any) ([]Boundary, string) {
			x := r.GetRepeated()
			uniq := uniqueKeys(f)
			rep := func(n uint64) []any {
				out := make([]any, n)
				for i := range out {
					out[i] = distinct(elem, uniq, i)
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

var reUniqueKey = regexp.MustCompile(`x\.([a-z_][a-z0-9_]*)`)

// uniqueKeys: 列表的元素要唯一的字段(消息契约的 唯一: CEL "this.map(x, …x.key…).unique()",R4)。
func uniqueKeys(f *ir.Field) []string {
	if f == nil || f.Rules == nil || !strings.Contains(string(f.Rules.Raw), ".unique()") {
		return nil
	}
	var out []string
	for _, m := range reUniqueKey.FindAllStringSubmatch(string(f.Rules.Raw), -1) {
		out = append(out, m[1])
	}
	return out
}

// distinct: 第 i 个元素(唯一键的字段换成不同的值,长度不变;i = 0 = 原样)。
func distinct(elem any, keys []string, i int) any {
	m, ok := elem.(map[string]any)
	if !ok || len(keys) == 0 || i == 0 {
		return elem
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	for _, k := range keys {
		switch v := cp[k].(type) {
		case string:
			suffix := "Z" + strconv.Itoa(i)
			if len(v) > len(suffix) {
				cp[k] = v[:len(v)-len(suffix)] + suffix
			} else {
				cp[k] = v + suffix
			}
		case int, int32, int64, float64:
			cp[k] = fmt.Sprint(v) + strconv.Itoa(i)
		}
	}
	return cp
}
