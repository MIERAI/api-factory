package kinds

func init() {
	// 邮编(日本): 文本 + 格式。数据里也按这个格式存
	Register(&Type{Name: "邮编", Base: "文本",
		Pattern: `^\d{3}-\d{4}$`,
		Boundaries: []Boundary{
			{"邮编格式不对(1234567,没有连字符)", "1234567", false},
			{"邮编格式正确(100-0001)", "100-0001", true},
		},
		Domain: []any{"100-0001", "100-0002", "530-0001", "060-0000"}, Missing: "999-9999"})
}
