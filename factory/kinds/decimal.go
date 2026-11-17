package kinds

import (
	"fmt"
	"math/big"

	"apifactory/factory/core"
)

func init() {
	// 十进制: 精确的十进制(numeric)。API 里是十进制字符串(保持登记时的位数),比较 / 排序按数值
	// 值域的陷阱: "10" 和 "9"(按文本比较 / 排序会颠倒)
	Register(&Type{Name: "十进制", Core: core.Decimal, SQL: []string{"numeric", "decimal"}, Proto: "string", Value: "decimal", Go: "string",
		Pattern: `^-?\d+(\.\d+)?$`,
		Boundaries: []Boundary{
			{"十进制格式不对(1e3)", "1e3", false},
			{"十进制格式正确(-12.50)", "-12.50", true},
		},
		Parse: func(s string) (any, error) {
			if _, ok := new(big.Rat).SetString(s); !ok {
				return nil, fmt.Errorf("%q 不是十进制数", s)
			}
			return s, nil // 保持原样的十进制写法
		},
		Domain: []any{big.NewRat(3, 2), big.NewRat(9, 1), big.NewRat(10, 1)}, Missing: big.NewRat(999, 1)})
}
