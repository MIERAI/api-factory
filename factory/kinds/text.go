package kinds

import "apifactory/factory/core"

func init() {
	// 值域的陷阱: 大小写(排序规则)、% _(前缀匹配的通配符)、非 ASCII、空字符串
	Register(&Type{Name: "文本", Core: core.Text, SQL: []string{"text", "varchar", "bpchar", "char"}, Proto: "string", Value: "string", Go: "string",
		Domain: []any{"a", "b", "B", "a%", "a_", "ab", "abc", "é"}, Missing: "zz-missing"})
}
