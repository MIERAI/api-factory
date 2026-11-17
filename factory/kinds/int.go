package kinds

import (
	"fmt"
	"strconv"

	"apifactory/factory/core"
)

func parseInt(s string) (any, error) {
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%q 不是整数", s)
	}
	return i, nil
}

func init() {
	Register(&Type{ID: "int32", Name: "整数32", Core: core.Int32, SQL: []string{"int4", "integer", "serial", "int2", "smallint"}, Proto: "int32", Value: "int",
		Parse: parseInt, Domain: []any{int64(0), int64(1), int64(2), int64(3), int64(10)}, Missing: int64(999)})
	Register(&Type{ID: "int64", Name: "整数64", Core: core.Int64, SQL: []string{"int8", "bigint", "bigserial"}, Proto: "int64", Value: "int",
		Parse: parseInt, Domain: []any{int64(0), int64(1), int64(2), int64(3), int64(10)}, Missing: int64(999)})
}
