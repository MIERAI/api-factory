package command

// 值的规范形(比较用): 模型状态、数据库读回的行、响应、例子的期待值都换成同一种写法再比较。
//   NULL → nil;整数 → 十进制整数的文本;十进制 → 数值(big.Rat 的 RatString,"1300.50" = "1300.5" = "2601/2");
//   时间 → UTC、微秒(Postgres 的精度)的 RFC 3339;日期 → YYYY-MM-DD;布尔 → true / false;文本 → 原样。
// 数据库生成、模型不知道的值 → Gen(和任何不为空的值相等,之后采用数据库的值)。

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Gen: 数据库生成的值(默认值 now()、序号 …)。
type Gen struct{}

func (Gen) String() string { return "<数据库生成>" }

var timeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05-07"}

// Canon: 列的类型(pg_type.typname)+ 值 → 规范形。
func Canon(typ string, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if _, ok := v.(Gen); ok {
		return v, nil
	}
	if n, ok := v.(json.Number); ok {
		v = string(n)
	}
	switch typ {
	case "int2", "int4", "int8":
		switch x := v.(type) {
		case int64:
			return strconv.FormatInt(x, 10), nil
		case int32:
			return strconv.FormatInt(int64(x), 10), nil
		case int:
			return strconv.Itoa(x), nil
		case float64:
			if x != float64(int64(x)) {
				return nil, fmt.Errorf("%v is not an integer", x)
			}
			return strconv.FormatInt(int64(x), 10), nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not an integer", x)
			}
			return strconv.FormatInt(n, 10), nil
		}
	case "numeric":
		switch x := v.(type) {
		case *big.Rat:
			return x.RatString(), nil
		case string:
			r, ok := new(big.Rat).SetString(strings.TrimSpace(x))
			if !ok {
				return nil, fmt.Errorf("%q is not a decimal", x)
			}
			return r.RatString(), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case int:
			return strconv.Itoa(x), nil
		case float64:
			return new(big.Rat).SetFloat64(x).RatString(), nil
		}
	case "timestamptz", "timestamp":
		switch x := v.(type) {
		case time.Time:
			return x.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), nil
		case string:
			for _, l := range timeLayouts {
				if t, err := time.Parse(l, strings.TrimSpace(x)); err == nil {
					return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), nil
				}
			}
			return nil, fmt.Errorf("%q is not a time", x)
		}
	case "date":
		switch x := v.(type) {
		case time.Time:
			return x.Format("2006-01-02"), nil
		case string:
			t, err := time.Parse("2006-01-02", strings.TrimSpace(x)[:min(10, len(strings.TrimSpace(x)))])
			if err != nil {
				return nil, fmt.Errorf("%q is not a date", x)
			}
			return t.Format("2006-01-02"), nil
		}
	case "bool":
		switch x := v.(type) {
		case bool:
			return strconv.FormatBool(x), nil
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true", "t":
				return "true", nil
			case "false", "f":
				return "false", nil
			}
			return nil, fmt.Errorf("%q is not a bool", x)
		}
	case "float8", "float4":
		switch x := v.(type) {
		case float64:
			return strconv.FormatFloat(x, 'g', -1, 64), nil
		case string:
			f, err := strconv.ParseFloat(x, 64)
			if err != nil {
				return nil, err
			}
			return strconv.FormatFloat(f, 'g', -1, 64), nil
		}
	default: // text / varchar …
		if s, ok := v.(string); ok {
			return s, nil
		}
		return fmt.Sprint(v), nil
	}
	return nil, fmt.Errorf("%s: value %v (%T) is not supported", typ, v, v)
}

// Equal: 两个规范值相等(Gen 和任何不为空的值相等)。
func Equal(a, b any) bool {
	_, ga := a.(Gen)
	_, gb := b.(Gen)
	switch {
	case ga || gb:
		return (ga || a != nil) && (gb || b != nil)
	case a == nil || b == nil:
		return a == nil && b == nil
	}
	return a == b
}

// Show: 规范值的显示(说明用)。
func Show(v any) string {
	if v == nil {
		return "NULL"
	}
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}
