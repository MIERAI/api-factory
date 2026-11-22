package core

// 聚合: 类型、NULL 的规则、SQL 的写法、参考实现 —— 只在这里定义一次。
// 用它的元件: group(顶层分组,本文件之外的 core.go / sql.go / eval.go),以后的 child_agg(按父行聚合)。
// 设计稿: docs/design/r2-relations.md §2.3 / §2.4
//
// 规则(元件认证 cert_test.go 逐条验证):
//
//	count()   行数。int64,不为 NULL。空集合 → 0(group 里空组不会出现)
//	sum(x)    x 为整数 → int64(溢出 → 查询失败);x 为十进制 → 十进制(精确)。
//	          忽略 NULL 的值;空集合 / 全部为 NULL → 0(不是 NULL: 业务上"合计 0 元")
//	min(x)    x 的类型。忽略 NULL;空集合 / 全部为 NULL → NULL。文本按字节比较(COLLATE "C")
//	max(x)    同上

import (
	"fmt"
	"math/big"
)

// Agg: 一个聚合(输出名 = 函数(列))。count 没有列。
type Agg struct {
	Name string `json:"name"`
	Func string `json:"func"`          // count / sum / min / max
	Col  string `json:"col,omitempty"` // count 不写
}

func Count(name string) Agg     { return Agg{Name: name, Func: "count"} }
func Sum(name, col string) Agg  { return Agg{Name: name, Func: "sum", Col: col} }
func Min(name, col string) Agg  { return Agg{Name: name, Func: "min", Col: col} }
func Max(name, col string) Agg  { return Agg{Name: name, Func: "max", Col: col} }
func AggFuncs() []string        { return []string{"count", "sum", "min", "max"} }
func isAggFunc(f string) bool   { return f == "count" || f == "sum" || f == "min" || f == "max" }
func aggNeedsCol(f string) bool { return f != "count" }
func aggSumKind(k Kind) (Kind, bool) { // sum 的结果的类型
	switch k {
	case Int32, Int64:
		return Int64, true
	case Decimal:
		return Decimal, true
	}
	return "", false
}

// AggType: 聚合的结果的类型。col = 列的类型(count 时 nil);emptySet = 集合可能为空(child_agg: 没有子行)。
// group 的组至少有一行: min / max 的列不为空时结果也不为空。
func AggType(fn string, col *Type, emptySet bool) (Type, error) {
	if !isAggFunc(fn) {
		return Type{}, fmt.Errorf("不认识的聚合 %q(只有 count / sum / min / max)", fn)
	}
	if fn == "count" {
		if col != nil {
			return Type{}, fmt.Errorf("count 不带列(数的是行数)")
		}
		return Type{Kind: Int64}, nil
	}
	if col == nil {
		return Type{}, fmt.Errorf("%s 需要列", fn)
	}
	switch fn {
	case "sum":
		k, ok := aggSumKind(col.Kind)
		if !ok {
			return Type{}, fmt.Errorf("sum 只能用于整数 / 十进制的列,实际是 %s", col.Kind)
		}
		return Type{Kind: k}, nil
	default: // min / max
		switch col.Kind {
		case Int32, Int64, Decimal, Text, Date, Timestamp:
		default:
			return Type{}, fmt.Errorf("%s 不能用于 %s 的列", fn, col.Kind)
		}
		return Type{Kind: col.Kind, Nullable: col.Nullable || emptySet}, nil
	}
}

// aggSQL: 聚合的 SQL。x = 列的 SQL 表达式(count 时不用)。
func aggSQL(a Agg, x string, col Type) string {
	switch a.Func {
	case "count":
		return "count(*)"
	case "sum":
		if col.Kind == Decimal {
			return fmt.Sprintf("coalesce(sum(%s), 0)", x)
		}
		// int4 的 sum 是 int8(int8 溢出时报错);int8 的 sum 是 numeric,cast 回 int8 时超出范围报错
		return fmt.Sprintf("coalesce(sum(%s), 0)::int8", x)
	}
	if col.Kind == Text {
		x += ` COLLATE "C"` // 按字节(和参考求值器一致;数据库默认的排序规则可能不是按字节)
	}
	return fmt.Sprintf("%s(%s)", a.Func, x)
}

// evalAgg: 参考实现。vals = 集合里每一行这一列的值(count 时只看个数)。
func evalAgg(a Agg, vals []any, col Type) (any, error) {
	switch a.Func {
	case "count":
		return int64(len(vals)), nil
	case "sum":
		if col.Kind == Decimal {
			s := new(big.Rat)
			for _, v := range vals {
				if v != nil {
					s.Add(s, v.(*big.Rat))
				}
			}
			return s, nil
		}
		s := new(big.Int)
		for _, v := range vals {
			if v != nil {
				s.Add(s, big.NewInt(v.(int64)))
			}
		}
		if !s.IsInt64() {
			return nil, fmt.Errorf("sum(%s): bigint out of range", a.Col)
		}
		return s.Int64(), nil
	case "min", "max":
		var best any
		for _, v := range vals {
			if v == nil {
				continue
			}
			if best == nil {
				best = v
				continue
			}
			c := compareValues(v, best)
			if (a.Func == "min" && c < 0) || (a.Func == "max" && c > 0) {
				best = v
			}
		}
		return best, nil
	}
	return nil, fmt.Errorf("eval: 不认识的聚合 %q", a.Func)
}
