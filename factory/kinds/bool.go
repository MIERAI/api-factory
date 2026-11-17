package kinds

import (
	"fmt"

	"apifactory/factory/core"
)

func init() {
	Register(&Type{Name: "布尔", Core: core.Bool, SQL: []string{"bool", "boolean"}, Proto: "bool", Value: "bool", Go: "bool",
		Parse: func(s string) (any, error) {
			switch s {
			case "true", "是":
				return true, nil
			case "false", "否":
				return false, nil
			}
			return nil, fmt.Errorf("%q 不是 是/否", s)
		},
		Domain: []any{true, false}, Missing: true})
}
