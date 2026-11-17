package core

import "testing"

func TestNarrow(t *testing.T) {
	cols := Schema{{"c", Type{Kind: Text, Nullable: true}}, {"d", Type{Kind: Int32, Nullable: true}}}
	p := &Pipeline{Params: []ParamDecl{
		{Name: "req", Type: Type{Kind: Text}, List: true, Required: true},
		{Name: "opt", Type: Type{Kind: Text}},
		{Name: "n", Type: Type{Kind: Int32}, Required: true},
	}}
	nullable := func(pred *Expr) map[string]bool {
		out := map[string]bool{}
		for _, c := range narrow(cols, pred, p) {
			out[c.Name] = c.Type.Nullable
		}
		return out
	}
	for _, c := range []struct {
		name string
		pred *Expr
		c, d bool // 收窄之后还可空吗
	}{
		{"必填的属于", Or(Absent("req"), InList(Col("c"), "req")), false, true},
		{"可选的等于: 不传时恒为真,不能收窄", Or(Absent("opt"), Cmp("=", Col("c"), Param("opt"))), true, true},
		{"not is_null", Not(IsNull(Col("d"))), true, false},
		{"and: 各项的并集", And(Or(Absent("req"), InList(Col("c"), "req")), Cmp(">", Col("d"), Param("n"))), false, false},
		{"or: 各项的交集", Or(Cmp("=", Col("c"), Param("opt")), Cmp("=", Col("d"), Param("n"))), true, true},
		{"is_null 本身不拒绝", IsNull(Col("c")), true, true},
	} {
		got := nullable(c.pred)
		if got["c"] != c.c || got["d"] != c.d {
			t.Errorf("%s: c 可空=%v d 可空=%v,应该是 %v %v", c.name, got["c"], got["d"], c.c, c.d)
		}
	}
}
