package compiler

import (
	"strings"
	"testing"

	"apifactory/factory/ir"
)

// 同时抢: 是 的形状检查: 支持的形状通过;不支持的每一种都拒绝,消息里说为什么。
func TestBucketShape(t *testing.T) {
	cat := &catalog{tables: map[string][]catColumn{
		"stock": {{Name: "sku", Type: "text"}, {Name: "qty", Type: "int4"}, {Name: "price", Type: "numeric"}, {Name: "touched", Type: "timestamptz"}, {Name: "version", Type: "int4"}},
	}}
	keys := map[string][][]string{"stock": {{"sku"}}}
	in := func(n string) *ir.CmdExpr { return &ir.CmdExpr{Op: "input", Name: n, Type: "int4"} }
	col := func(n string) *ir.CmdExpr { return &ir.CmdExpr{Op: "col", Name: n} }
	take := func(mod func(op *ir.CommandOp)) []ir.API {
		op := ir.CommandOp{Name: "取", Kind: "update", Table: "stock",
			Key:    []ir.CommandVal{{Column: "sku", Type: "text", Expr: &ir.CmdExpr{Op: "input", Name: "sku"}}},
			Values: []ir.CommandVal{{Column: "qty", Type: "int4", Expr: &ir.CmdExpr{Op: "-", Args: []*ir.CmdExpr{col("qty"), in("n")}}}, {Column: "touched", Type: "timestamptz", Expr: &ir.CmdExpr{Op: "now"}}},
			Cond:   &ir.CmdExpr{Op: ">=", Args: []*ir.CmdExpr{col("qty"), in("n")}}}
		if mod != nil {
			mod(&op)
		}
		return []ir.API{{ID: "x.take", Command: &ir.Command{Op: "command", Ops: []ir.CommandOp{op}}}}
	}
	if b, why := bucketShape("stock", "qty", nil, take(nil), cat, keys); why != "" || strings.Join(b.PerBucket, ",") != "touched" {
		t.Fatalf("支持的形状被拒绝: %q %+v", why, b)
	}
	for name, c := range map[string]struct {
		apis []ir.API
		col  string
		want string
	}{
		"没有条件":     {take(func(op *ir.CommandOp) { op.Cond = nil }), "qty", "条件"},
		"条件的量不同":   {take(func(op *ir.CommandOp) { op.Cond.Args[1] = in("m") }), "qty", "条件"},
		"同时改别的列":   {take(func(op *ir.CommandOp) { op.Values = append(op.Values, ir.CommandVal{Column: "price", Expr: in("p")}) }), "qty", "price"},
		"乐观锁":      {take(func(op *ir.CommandOp) { op.Lock = &ir.CommandVal{Column: "version", Expr: in("v")} }), "qty", "乐观锁"},
		"不是按主键":    {take(func(op *ir.CommandOp) { op.Key[0].Column = "price" }), "qty", "主键"},
		"数量不是整数":   {take(nil), "price", "整数"},
		"设成一个值":    {take(func(op *ir.CommandOp) { op.Values[0].Expr = in("n") }), "qty", "设成一个值"},
		"幂等新建":     {append(take(nil), ir.API{ID: "x.add", Command: &ir.Command{Op: "command", Ops: []ir.CommandOp{{Name: "加", Kind: "insert_idempotent", Table: "stock"}}}}), "qty", "幂等"},
		"新建之后返回数量": {append(take(nil), ir.API{ID: "x.add", Command: &ir.Command{Op: "command", Ops: []ir.CommandOp{{Name: "加", Kind: "insert", Table: "stock", Returning: []ir.Column{{Name: "qty"}}}}}}), "qty", "返回"},
	} {
		if _, why := bucketShape("stock", c.col, nil, c.apis, cat, keys); !strings.Contains(why, c.want) {
			t.Errorf("%s: 应该拒绝(含 %q),实际 %q", name, c.want, why)
		}
	}
}
