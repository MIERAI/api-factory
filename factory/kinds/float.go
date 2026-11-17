package kinds

import (
	"fmt"
	"strconv"
)

func init() {
	// 小数: 二进制浮点(double)。只作为计算的输出(没有核心类型,不能做筛选 / 排序)
	Register(&Type{Name: "小数", SQL: []string{"float8", "double precision", "float4", "real"}, Proto: "double", Value: "float", Go: "float64",
		Parse: func(s string) (any, error) {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("%q 不是数字", s)
			}
			return f, nil
		}})
}
