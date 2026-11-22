package command

import (
	"fmt"
	"strings"
	"testing"

	"apifactory/factory/ir"
)

func testScope(rowCols bool) Scope {
	cols := map[string]string{"available": "int4", "price": "numeric", "status": "int4", "sku": "text", "n8": "int8", "ok": "bool"}
	sc := Scope{
		Table: "stock",
		Ref: func(op, col string) (string, error) {
			if op == "库存" && col == "id" {
				return "int8", nil
			}
			return "", fmt.Errorf("no %s.%s", op, col)
		},
		Input: func(name string) (string, error) {
			t, ok := map[string]string{"delta": "int4", "big": "int8", "amount": "numeric", "s": "enum", "sku": "text"}[name]
			if !ok {
				return "", fmt.Errorf("no input %s", name)
			}
			return t, nil
		},
		Enum: func(table, col, name string) (string, bool) {
			if col == "status" && name == "HELD" {
				return "1", true
			}
			return "", false
		},
		EnumOf: func(table, col string) bool { return col == "status" },
	}
	if rowCols {
		sc.Column = func(n string) (string, bool) { t, ok := cols[n]; return t, ok }
	}
	return sc
}

func TestConditions(t *testing.T) {
	ok := map[string]string{ // 设计稿的写法 → 规范化之后(spec)
		"available + 输入.delta >= 0":           "available + 输入.delta >= 0",
		"available>=输入.delta":                 "available >= 输入.delta",
		"status = HELD 并且 available - 1 >= 0": "status = 1 并且 available - 1 >= 0",
		"status = 输入.s":                       "status = 输入.s",
		"price >= 输入.amount":                  "price >= 输入.amount",
		"price + 1.5 > 0":                     "price + 1.5 > 0",
		"sku <> 'x''y'":                       "sku <> 'x''y'",
		"available > -1":                      "available > -1",
	}
	for src, want := range ok {
		x, err := ParseExpr(src, true)
		if err == nil {
			err = Resolve(x, testScope(true), "", "", "")
		}
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got := Format(x); got != want {
			t.Errorf("%s: got %q, want %q", src, got, want)
		}
	}
	bad := map[string]string{
		"输入.delta >= 0":         "要用到这一行的列",
		"sku > 'a'":             "只能用 = / <>",
		"status = 3":            "写值名",
		"status = DONE":         "不是枚举",
		"available >= 输入.big":   "比对方(int4)宽",
		"available + sku > 0":   "加减只用于",
		"available = 输入.amount": "", // int 和 numeric 可以比较
		"available >= ":         "不完整",
		"available ~ 1":         "无法识别",
		"nope > 1":              "至少要有一个列",
		"available + 输入.x > 0":  "no input",
		"库存.id > 1 或者 x":        "多余",
	}
	for src, want := range bad {
		x, err := ParseExpr(src, true)
		if err == nil {
			err = Resolve(x, testScope(true), "", "", "")
		}
		if want == "" {
			if err != nil {
				t.Errorf("%s: %v", src, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", src, want, err)
		}
	}
}

func TestValues(t *testing.T) {
	cases := []struct {
		src, want, col string
		rowCols        bool
		bad            string
	}{
		{"available + 输入.delta", "int4", "int4", true, ""},
		{"库存.id", "int8", "int8", false, ""},
		{"现在", "timestamptz", "timestamptz", false, ""},
		{"HELD", "", "enum:stock.status", false, ""},
		{"available", "", "int4", false, "不能引用列"},
		{"库存.id", "", "int4", false, "不能写进"},
		{"输入.amount", "", "int4", false, "不能写进"},
		{"其他.id", "", "int8", false, "no 其他.id"},
	}
	for _, c := range cases {
		x, err := ParseExpr(c.src, false)
		if err != nil {
			t.Fatalf("%s: %v", c.src, err)
		}
		wt, wc := "", ""
		if strings.HasPrefix(c.col, "enum:") {
			wt, wc = "stock", "status"
		}
		err = Resolve(x, testScope(c.rowCols), c.col, wt, wc)
		switch {
		case c.bad == "" && err != nil:
			t.Errorf("%s: %v", c.src, err)
		case c.bad != "" && (err == nil || !strings.Contains(err.Error(), c.bad)):
			t.Errorf("%s: want error %q, got %v", c.src, c.bad, err)
		case c.bad == "" && c.want != "" && x.Type != c.want:
			t.Errorf("%s: type %s, want %s", c.src, x.Type, c.want)
		}
	}
	// spec 的写法(枚举已经是数据库值)在编译器里重新解析: 不需要 Enum
	x, _ := ParseExpr("status = 1", true)
	sc := testScope(true)
	sc.Enum = nil
	if err := Resolve(x, sc, "", "", ""); err != nil {
		t.Errorf("normalized enum: %v", err)
	}
}

// 模型: 条件更新的判定顺序、SET 用更新之前的值、幂等新建、后面的操作引用前面的返回值、失败时状态不变。
func TestRun(t *testing.T) {
	stock := &Table{Name: "stock", PK: []string{"sku"}, Uniques: [][]string{{"sku"}},
		Cols: []Column{{Name: "sku", Type: "text", NotNull: true}, {Name: "n", Type: "int4", NotNull: true}, {Name: "version", Type: "int4", NotNull: true}}}
	logs := &Table{Name: "logs", PK: []string{"id"}, Uniques: [][]string{{"id"}, {"ref"}},
		Cols: []Column{{Name: "id", Type: "int8", NotNull: true, Default: &Default{Generated: true}}, {Name: "sku", Type: "text", NotNull: true}, {Name: "ref", Type: "text"}}}
	st := &State{Tables: map[string]*TableState{
		"stock": {T: stock, Rows: []Row{{"sku": "A", "n": "5", "version": "1"}}},
		"logs":  {T: logs},
	}}
	in := func(n string) *ir.CmdExpr {
		return &ir.CmdExpr{Op: "input", Name: n, Type: map[string]string{"sku": "text", "d": "int4", "v": "int4", "ref": "text"}[n]}
	}
	col := func(n string) *ir.CmdExpr { return &ir.CmdExpr{Op: "col", Name: n, Type: "int4"} }
	sum := &ir.CmdExpr{Op: "+", Type: "int4", Args: []*ir.CmdExpr{col("n"), in("d")}}
	upd := ir.CommandOp{Name: "扣", Kind: "update", Table: "stock",
		Key:    []ir.CommandVal{{Column: "sku", Type: "text", Expr: in("sku")}},
		Values: []ir.CommandVal{{Column: "n", Type: "int4", Expr: sum}},
		Lock:   &ir.CommandVal{Column: "version", Type: "int4", Expr: in("v")},
		Cond:   &ir.CmdExpr{Op: ">=", Args: []*ir.CmdExpr{sum, {Op: "const", Value: "0", Type: "int4"}}}, Unmet: "LOW"}
	ins := ir.CommandOp{Name: "记", Kind: "insert_idempotent", Table: "logs", Conflict: []string{"ref"},
		Values: []ir.CommandVal{{Column: "sku", Type: "text", Expr: &ir.CmdExpr{Op: "ref", Ref: "扣", Name: "sku", Type: "text"}}, {Column: "ref", Type: "text", Expr: in("ref")}}}
	ops := []ir.CommandOp{upd, ins}
	run := func(sku string, d, v int, ref string) ProgResult {
		return st.Run(ops, Inputs{"sku": sku, "d": d, "v": v, "ref": ref}, "2026-11-22T00:00:00Z")
	}
	if r := run("Z", -1, 1, "r1"); r.Outcome != NotFound || r.Failed != 0 {
		t.Errorf("missing: %+v", r)
	}
	if r := run("A", -9, 2, "r1"); r.Outcome != Conflict { // 版本 先于 条件
		t.Errorf("stale version + unmet: %+v", r)
	}
	if r := run("A", -9, 1, "r1"); r.Outcome != Unmet || r.Reason != "LOW" || r.Code() != "FAILED_PRECONDITION" {
		t.Errorf("unmet: %+v", r)
	}
	r := run("A", -5, 1, "r1")
	if r.Outcome != OK || r.Ops[0].Row["n"] != "0" || r.Ops[0].Row["version"] != "2" || !r.Ops[1].Created || r.Ops[1].Row["sku"] != "A" {
		t.Fatalf("ok: %+v", r)
	}
	if r := run("A", 0, 2, "r1"); r.Outcome != OK || r.Ops[1].Created { // 幂等命中: 已有的行
		t.Errorf("hit: %+v", r)
	}
	if n := len(st.Tables["logs"].Rows); n != 1 {
		t.Errorf("logs: %d rows", n)
	}
	before := st.Tables["stock"].Rows[0]["n"]
	if r := run("A", -1, 3, "r2"); r.Outcome != Unmet { // n = 0: 失败 → 状态不变
		t.Errorf("unmet at 0: %+v", r)
	}
	if st.Tables["stock"].Rows[0]["n"] != before || len(st.Tables["logs"].Rows) != 1 {
		t.Error("a failed command changed the state")
	}
	big := run("A", 2147483647, 3, "r3") // 0 + max = max: 没有溢出
	over := st.Clone().Run(ops, Inputs{"sku": "A", "d": 1, "v": 4, "ref": "r4"}, "2026-11-22T00:00:00Z")
	if big.Outcome != OK || over.Outcome != Check {
		t.Errorf("int4 overflow: %s %s", big.Outcome, over.Outcome)
	}
}
