package kinds

func init() {
	// 非负十进制: 十进制 + 不能是负数(价格、金额、数量之类;数据库里常见的 CHECK (x >= 0))。
	// 写入的 API(命令)用它: 请求的校验要拦住数据库的规则会拒绝的值
	Register(&Type{ID: "nonneg_decimal", Name: "非负十进制", Base: "十进制",
		Pattern: `^\d+(\.\d+)?$`,
		Boundaries: []Boundary{
			{"非负十进制是负数(-0.01)", "-0.01", false},
			{"非负十进制格式不对(1e3)", "1e3", false},
			{"非负十进制的最小值(0)", "0", true},
		}})
}
