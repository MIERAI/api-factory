package perf

import (
	"testing"

	"apifactory/factory/ir"
)

func in(name, typ string) *ir.CmdExpr   { return &ir.CmdExpr{Op: "input", Name: name, Type: typ} }
func item(name, typ string) *ir.CmdExpr { return &ir.CmdExpr{Op: "item", Name: name, Type: typ} }
func col(name string) *ir.CmdExpr       { return &ir.CmdExpr{Op: "col", Name: name} }

// Keys first, then versions, equality conditions, inserts; amounts compared / subtracted are the constant 1.
func TestBindInputs(t *testing.T) {
	ops := []ir.CommandOp{
		{Name: "ins", Kind: "insert_idempotent", Table: "debits", Values: []ir.CommandVal{
			{Column: "transfer_id", Type: "int8", Expr: in("transfer_id", "int8")},
			{Column: "account_id", Type: "text", Expr: item("account_id", "text")},
			{Column: "amount", Type: "int8", Expr: item("amount", "int8")},
		}},
		{Name: "upd", Kind: "update", Table: "accounts",
			Key:    []ir.CommandVal{{Column: "account_id", Type: "text", Expr: item("account_id", "text")}},
			Values: []ir.CommandVal{{Column: "balance", Type: "int8", Expr: &ir.CmdExpr{Op: "-", Args: []*ir.CmdExpr{col("balance"), item("amount", "int8")}}}},
			Cond: &ir.CmdExpr{Op: "and", Args: []*ir.CmdExpr{
				{Op: ">=", Args: []*ir.CmdExpr{col("balance"), item("amount", "int8")}},
				{Op: "=", Args: []*ir.CmdExpr{col("revision"), in("revision", "int4")}},
			}},
			Lock: &ir.CommandVal{Column: "version", Type: "int4", Expr: in("version", "int4")},
		},
	}
	got := bindInputs(ops)
	want := map[string]binding{
		"item:account_id":   {"accounts", "account_id", "key", "text"},
		"input:revision":    {"accounts", "revision", "key", "int4"},
		"input:version":     {"accounts", "version", "version", "int4"},
		"input:transfer_id": {"debits", "transfer_id", "new", "int8"},
		"item:amount":       {kind: "one", typ: "int8"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v want %+v", k, got[k], w)
		}
	}
}

// 每项: consecutive operations of one list run per item, in order.
func TestSteps(t *testing.T) {
	ops := []ir.CommandOp{{Name: "a"}, {Name: "b", Each: "lines"}, {Name: "c", Each: "lines"}, {Name: "d"}}
	env := &reqEnv{items: []map[string]any{{}, {}}}
	got := steps(ops, env)
	want := [][2]int{{0, -1}, {1, 0}, {2, 0}, {1, 1}, {2, 1}, {3, -1}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
