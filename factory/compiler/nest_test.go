package compiler

import (
	"testing"

	"apifactory/factory/spec"
)

func TestChildView(t *testing.T) {
	cat := &catalog{tables: map[string][]catColumn{
		"orders":      {{"id", "int8", true}, {"customer_id", "int8", true}, {"store", "int4", true}},
		"order_items": {{"order_id", "int8", true}, {"line_no", "int4", true}, {"sku", "text", true}, {"price", "numeric", true}, {"note", "text", false}, {"store", "int4", true}},
	}}
	keys := map[string][][]string{"orders": {{"id"}}, "order_items": {{"order_id", "line_no"}}}
	mk := func(shard map[string]string, ns []spec.Nest, as []spec.ChildAgg) *spec.Spec {
		d := &spec.Domain{Entities: map[string]spec.Entity{}}
		for t, c := range shard {
			d.Entities[t] = spec.Entity{ShardBy: c}
		}
		return &spec.Spec{Domain: d, Datasource: spec.Datasource{Table: "orders", Nests: ns, ChildAggs: as}}
	}
	items := spec.Nest{Name: "items", Table: "order_items", On: []string{"id = order_id"}, Where: []string{"note 不为空"}, OrderBy: []string{"line_no asc"}, Max: 50,
		Columns: []spec.TakeCol{{Name: "line_no", From: "line_no"}, {Name: "amount", From: "price"}}}
	aggs := []spec.ChildAgg{
		{Name: "n", Table: "order_items", On: []string{"id = order_id"}, Func: "count"},
		{Name: "total", Table: "order_items", On: []string{"id = order_id"}, Func: "sum", Column: "price"},
		{Name: "first_sku", Table: "order_items", On: []string{"id = order_id"}, Func: "min", Column: "sku"},
	}
	view, steps, err := lookupView(mk(nil, []spec.Nest{items}, aggs), cat, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 4 || steps[0].Kind != "nest" || steps[0].N != 50 || steps[0].Pred == nil || steps[1].Kind != "child_agg" {
		t.Fatalf("steps: %+v", steps)
	}
	want := map[string]catColumn{"items": {"items", NestType, true}, "n": {"n", "int8", true}, "total": {"total", "numeric", true}, "first_sku": {"first_sku", "text", false}}
	for _, c := range view.tables["orders"] {
		if w, ok := want[c.Name]; ok && w != c {
			t.Errorf("view %s: %+v, want %+v", c.Name, c, w)
		}
	}
	bad := map[string]*spec.Spec{
		"顺序不是全序": mk(nil, []spec.Nest{{Name: "items", Table: "order_items", On: []string{"id = order_id"}, OrderBy: []string{"sku asc"}, Max: 5, Columns: items.Columns}}, nil),
		"子表不存在":  mk(nil, []spec.Nest{{Name: "items", Table: "nope", On: []string{"id = order_id"}, OrderBy: []string{"line_no asc"}, Max: 5, Columns: items.Columns}}, nil),
		"重名":     mk(nil, nil, []spec.ChildAgg{{Name: "store", Table: "order_items", On: []string{"id = order_id"}, Func: "count"}}),
		"sum 文本": mk(nil, nil, []spec.ChildAgg{{Name: "x", Table: "order_items", On: []string{"id = order_id"}, Func: "sum", Column: "sku"}}),
		"只有子表分库": mk(map[string]string{"order_items": "store"}, nil, aggs[:1]),
		"分库键不相等": mk(map[string]string{"orders": "store", "order_items": "store"}, nil, aggs[:1]),
	}
	for name, s := range bad {
		if _, _, err := lookupView(s, cat, keys, nil); err == nil {
			t.Errorf("%s: 应该报错", name)
		}
	}
	ok := mk(map[string]string{"orders": "store", "order_items": "store"}, nil,
		[]spec.ChildAgg{{Name: "n", Table: "order_items", On: []string{"id = order_id", "store = store"}, Func: "count"}})
	if _, _, err := lookupView(ok, cat, keys, nil); err != nil {
		t.Errorf("分库键相等: %v", err)
	}
}
