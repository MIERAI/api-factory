package constraints

// 格式: 普通文本的字符种类。设计语言 格式: 数字 / 英数字(列表时用于每个元素)。
// 带格式的类型(日期 / 邮编 …)本身已经有格式,不能再加。

import (
	"errors"
	"fmt"
	"strings"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"

	"apifactory/factory/ir"
)

type textFormat struct {
	name    string
	pattern string
	valid   []string // 合法的例子(第一个用作列表的元素)
	invalid []string // 违反的例子
	domain  []any    // 差分测试的值域
	missing string   // 数据里不存在、但合法的值
}

var textFormats = []textFormat{
	{name: "数字", pattern: "^[0-9]+$", valid: []string{"0123"}, invalid: []string{"12a", "１２", "1 2", "-1"},
		domain: []any{"1", "2", "10", "0123", "4901234567890"}, missing: "99999999"},
	{name: "英数字", pattern: "^[0-9A-Za-z]+$", valid: []string{"A01b"}, invalid: []string{"A-1", "Ａ1", "a b"},
		domain: []any{"A1", "a1", "B2", "10", "Z"}, missing: "ZZZZ9999"},
}

func formatByPattern(p string) *textFormat {
	for i := range textFormats {
		if textFormats[i].pattern == p {
			return &textFormats[i]
		}
	}
	return nil
}

func init() {
	register(&Constraint{
		Key: "格式", Level: Elem, Example: "数字 / 英数字",
		Compile: func(c Ctx, v string) ([]string, error) {
			if c.ProtoType != "string" {
				return nil, errors.New("只能用于文本")
			}
			if c.HasPattern {
				return nil, errors.New("这个类型本身已经有格式")
			}
			for _, f := range textFormats {
				if f.name == v {
					return []string{fmt.Sprintf("string.pattern = %q", f.pattern)}, nil
				}
			}
			return nil, errors.New("无法识别")
		},
		Boundaries: func(r *validate.FieldRules, _ *ir.Field, _ any) ([]Boundary, string) {
			f := formatByPattern(r.GetString().GetPattern())
			if f == nil {
				return nil, ""
			}
			var b []Boundary
			x := r.GetString()
			for _, v := range f.valid {
				// 合法的例子也要满足同一字段的长度: 补齐 / 截断(用例子的第一个字符)
				n := len([]rune(v))
				if x.HasMinLen() && uint64(n) < x.GetMinLen() {
					v += strings.Repeat(string([]rune(v)[0]), int(x.GetMinLen())-n)
				}
				if x.HasMaxLen() && uint64(len([]rune(v))) > x.GetMaxLen() {
					v = string([]rune(v)[:x.GetMaxLen()])
				}
				b = append(b, Boundary{fmt.Sprintf("= %q(格式 %s,合法)", v, f.name), v, true, false})
			}
			for _, v := range f.invalid {
				b = append(b, Boundary{fmt.Sprintf("= %q(格式 %s,违反)", v, f.name), v, false, false})
			}
			return b, "格式 " + f.name
		},
		Domain: func(r *validate.FieldRules) *Domain {
			f := formatByPattern(r.GetString().GetPattern())
			if f == nil {
				return nil
			}
			prefix := map[string]string{"数字": "", "英数字": "K"}[f.name]
			return &Domain{Values: f.domain, Missing: f.missing, Expand: func(i int) any { return fmt.Sprintf("%s%d", prefix, 100+i) }}
		},
	})
}
