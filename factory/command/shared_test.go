package command

import (
	"testing"

	"apifactory/factory/ir"
)

func TestSharedQuantity(t *testing.T) {
	col := func(n string) *ir.CmdExpr { return &ir.CmdExpr{Op: "col", Name: n} }
	in := &ir.CmdExpr{Op: "input", Name: "n"}
	ops := []ir.CommandOp{
		{Name: "a", Kind: "update", Table: "t", Values: []ir.CommandVal{{Column: "q", Type: "int4", Expr: &ir.CmdExpr{Op: "-", Args: []*ir.CmdExpr{col("q"), in}}}}},
		{Name: "b", Kind: "update", Table: "t", Values: []ir.CommandVal{{Column: "q", Type: "int8", Expr: &ir.CmdExpr{Op: "+", Args: []*ir.CmdExpr{in, col("q")}}}}},
		{Name: "c", Kind: "update", Table: "t", Values: []ir.CommandVal{{Column: "q", Type: "int4", Expr: in}}},                                                     // 设成一个值: 不是
		{Name: "d", Kind: "update", Table: "t", Values: []ir.CommandVal{{Column: "s", Type: "text", Expr: &ir.CmdExpr{Op: "+", Args: []*ir.CmdExpr{col("s"), in}}}}}, // 不是数值
		{Name: "e", Kind: "update", Table: "t", Values: []ir.CommandVal{{Column: "q", Type: "int4", Expr: &ir.CmdExpr{Op: "-", Args: []*ir.CmdExpr{in, col("q")}}}}}, // n - q: 不是
		{Name: "f", Kind: "insert", Table: "t", Values: []ir.CommandVal{{Column: "q", Type: "int4", Expr: &ir.CmdExpr{Op: "+", Args: []*ir.CmdExpr{col("q"), in}}}}},
	}
	got := SharedQuantity(ops)
	if len(got) != 2 || got[0].Op != "a" || got[1].Op != "b" {
		t.Fatalf("%+v", got)
	}
}
