package command

import (
	"strings"

	"apifactory/factory/ir"
)

// SharedQuantity: 共享数量的读改写(docs/design/hotspots.md §2): 按键更新的操作里 列 = 列 ± 表达式(整数 / 十进制的列)。
// 库存扣减、计数器、余额 …: 很多请求同时改同一个键时排队(行锁)。工厂只问这些 API 一个业务问题(同时抢)。
func SharedQuantity(ops []ir.CommandOp) []ir.SharedQty {
	var out []ir.SharedQty
	for _, op := range ops {
		if op.Kind != "update" {
			continue
		}
		for _, v := range op.Values {
			if _, ok := SelfDelta(v); ok && numericType(v.Type) {
				out = append(out, ir.SharedQty{Table: op.Table, Column: v.Column, Op: op.Name})
			}
		}
	}
	return out
}

// SelfDelta: 列 ← 列 + x / 列 − x 时返回 ("+" 或 "-", x)。
func SelfDelta(v ir.CommandVal) (string, bool) {
	e := v.Expr
	if e == nil || (e.Op != "+" && e.Op != "-") || len(e.Args) != 2 {
		return "", false
	}
	if a := e.Args[0]; a.Op == "col" && a.Name == v.Column {
		return e.Op, true
	}
	if b := e.Args[1]; e.Op == "+" && b.Op == "col" && b.Name == v.Column {
		return e.Op, true
	}
	return "", false
}

// Delta: SelfDelta 的 x(列 ± x 里的 x)。
func Delta(v ir.CommandVal) *ir.CmdExpr {
	e := v.Expr
	if a := e.Args[0]; a.Op == "col" && a.Name == v.Column {
		return e.Args[1]
	}
	return e.Args[0]
}

func numericType(t string) bool {
	switch strings.ToLower(t) {
	case "int2", "int4", "int8", "smallint", "integer", "bigint", "numeric":
		return true
	}
	return false
}
