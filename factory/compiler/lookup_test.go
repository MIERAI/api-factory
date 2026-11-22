package compiler

import (
	"fmt"
	"strings"
	"testing"

	"apifactory/factory/cells"
	"apifactory/factory/spec"
)

func TestParseUniqueKeys(t *testing.T) {
	src := `
-- 注释里的 UNIQUE (x) 不算
CREATE TABLE IF NOT EXISTS a (
    id   BIGINT PRIMARY KEY,
    sku  TEXT NOT NULL UNIQUE CHECK (char_length(sku) <= 32),
    note TEXT CHECK (note <> 'UNIQUE'),
    unique_no INT
);
CREATE TABLE b (
    x INT NOT NULL, y TEXT NOT NULL, z INT,
    PRIMARY KEY (x, y),
    CONSTRAINT b_z UNIQUE (z, x)
);
CREATE UNIQUE INDEX IF NOT EXISTS b_y_idx ON b (y);
CREATE UNIQUE INDEX b_part ON b (z) WHERE z > 0;
CREATE UNIQUE INDEX b_expr ON b (lower(y));
CREATE INDEX b_plain ON b (z);
`
	out := map[string][][]string{}
	parseUniqueKeys(src, out)
	if got, want := fmt.Sprint(out["a"]), "[[id] [sku]]"; got != want {
		t.Errorf("a: %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(out["b"]), "[[x y] [z x] [y]]"; got != want {
		t.Errorf("b: %s, want %s", got, want)
	}
}

func TestLookupView(t *testing.T) {
	cat := &catalog{tables: map[string][]catColumn{
		"orders":    {{"id", "int8", true}, {"customer_id", "int8", false}, {"store", "int4", true}},
		"customers": {{"id", "int8", true}, {"name", "text", true}, {"rank", "int4", false}, {"store", "int4", true}},
		"ranks":     {{"rank", "int4", true}, {"label", "text", true}},
	}}
	keys := map[string][][]string{"customers": {{"id"}}, "ranks": {{"rank"}}}
	mk := func(shard map[string]string, ls ...spec.Lookup) *spec.Spec {
		d := &spec.Domain{Entities: map[string]spec.Entity{}}
		for t, c := range shard {
			d.Entities[t] = spec.Entity{ShardBy: c}
		}
		return &spec.Spec{Domain: d, Datasource: spec.Datasource{Table: "orders", Lookups: ls}}
	}
	cust := spec.Lookup{Name: "c", Table: "customers", On: []string{"customer_id = id"}, Columns: []spec.TakeCol{{Name: "cname", From: "name"}, {Name: "crank", From: "rank"}}}
	rank := spec.Lookup{Name: "r", Table: "ranks", On: []string{"crank = rank"}, Columns: []spec.TakeCol{{Name: "rlabel", From: "label"}}, Missing: "drop"}
	view, steps, err := lookupView(mk(nil, cust, rank), cat, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[1].On[0].Col != "crank" || steps[0].Missing != "keep" {
		t.Errorf("steps: %+v", steps)
	}
	got := fmt.Sprint(view.tables["orders"][3:])
	if want := "[{cname text false} {crank int4 false} {rlabel text true}]"; got != want { // keep → 可空;drop → 保持
		t.Errorf("view: %s, want %s", got, want)
	}
	if len(cat.tables["orders"]) != 3 {
		t.Errorf("原来的目录被改了")
	}
	for name, c := range map[string]struct {
		s    *spec.Spec
		want string
	}{
		"不是唯一键":    {mk(nil, spec.Lookup{Name: "x", Table: "customers", On: []string{"store = store"}, Columns: []spec.TakeCol{{Name: "n", From: "name"}}}), "不是主键或唯一约束"},
		"列不存在":     {mk(nil, spec.Lookup{Name: "x", Table: "customers", On: []string{"nope = id"}, Columns: []spec.TakeCol{{Name: "n", From: "name"}}}), "不是 orders 的列"},
		"重名":       {mk(nil, spec.Lookup{Name: "x", Table: "customers", On: []string{"customer_id = id"}, Columns: []spec.TakeCol{{Name: "store", From: "name"}}}), "重名"},
		"条件的写法":    {mk(nil, spec.Lookup{Name: "x", Table: "customers", On: []string{"customer_id"}, Columns: []spec.TakeCol{{Name: "n", From: "name"}}}), "写成"},
		"不分库关联分库":  {mk(map[string]string{"customers": "store"}, cust), "不分库的表不能关联分库的表"},
		"分库关联不分库":  {mk(map[string]string{"orders": "store"}, cust), "只能关联同一个分库里的表"},
		"分库键不在条件里": {mk(map[string]string{"orders": "store", "customers": "store"}, cust), "两边的分库键相等"},
	} {
		_, _, err := lookupView(c.s, cat, keys, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	ok := cust
	ok.On = []string{"customer_id = id", "store = store"}
	if _, _, err := lookupView(mk(map[string]string{"orders": "store", "customers": "store"}, ok), cat, keys, nil); err != nil {
		t.Errorf("同一个分库的关联: %v", err)
	}
}

// 关联 / 子表是逻辑表(存储: JSON版本): 对方 = 来源的子流水线,唯一键 = 自然键;按非键的列关联 → 不是多对一,报错。
func TestLookupLogicalTable(t *testing.T) {
	cat := &catalog{tables: map[string][]catColumn{
		"labels":      {{"code", "text", true}, {"text", "text", false}},
		"products":    {{"code", "text", true}, {"name", "text", false}, {"cat", "text", false}},
		"prod_events": {{"id", "int8", true}, {"data", "jsonb", true}, {"action_type", "int4", true}, {"registered_at", "timestamptz", true}},
	}}
	st := &spec.Storage{Kind: "json_versions", Table: "prod_events", DataColumn: "data", ActionColumn: "action_type", DeleteValue: 1,
		OrderBy: []string{"registered_at", "id"}, Fields: []spec.StorageField{{Name: "code", Type: "text"}, {Name: "name", Type: "text"}, {Name: "cat", Type: "text"}}}
	d := &spec.Domain{Entities: map[string]spec.Entity{"products": {NaturalKey: []string{"code"}, Storage: st}}}
	lib, err := cells.Load()
	if err != nil {
		t.Fatal(err)
	}
	s := &spec.Spec{Domain: d, Datasource: spec.Datasource{Table: "labels", Lookups: []spec.Lookup{{Name: "p", Table: "products", On: []string{"code = code"}, Columns: []spec.TakeCol{{Name: "pname", From: "name"}}}}}}
	_, steps, err := lookupView(s, cat, map[string][][]string{}, lib)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || len(steps[0].Source) == 0 || steps[0].Source[0].Table != "prod_events" || fmt.Sprint(steps[0].Unique) != "[[code]]" {
		t.Fatalf("逻辑表的关联: %+v", steps)
	}
	s.Datasource.Lookups[0].On = []string{"text = cat"}
	if _, _, err := lookupView(s, cat, map[string][][]string{}, lib); err == nil || !strings.Contains(err.Error(), "不是主键或唯一约束") {
		t.Fatalf("按非键的列关联逻辑表应该报错: %v", err)
	}
}

// R2.6: 分库: 随父 / 对方的列 = 常量 / 按日期 / 嵌套里的关联 的编译规则。
func TestLookupR26(t *testing.T) {
	cat := &catalog{tables: map[string][]catColumn{
		"orders":    {{"id", "int8", true}, {"store", "int4", true}, {"sku", "text", true}, {"day", "date", true}},
		"lines":     {{"order_id", "int8", true}, {"no", "int4", true}, {"sku", "text", true}},
		"local":     {{"sku", "text", true}, {"name", "text", true}},
		"plain":     {{"sku", "text", true}, {"name", "text", true}},
		"rates":     {{"sku", "text", true}, {"kind", "text", true}, {"from_day", "date", true}, {"to_day", "date", false}, {"rate", "numeric", true}},
		"unsharded": {{"id", "int8", true}, {"sku", "text", true}},
	}}
	keys := map[string][][]string{"local": {{"sku"}}, "plain": {{"sku"}}, "rates": {{"sku", "kind", "from_day"}}, "lines": {{"order_id", "no"}}, "unsharded": {{"id"}}}
	dom := &spec.Domain{Entities: map[string]spec.Entity{
		"orders": {ShardBy: "store"}, "lines": {ShardWithParent: true}, "local": {ShardWithParent: true},
	}}
	look := func(table string, on ...string) spec.Lookup {
		return spec.Lookup{Name: "x", Table: table, On: on, Columns: []spec.TakeCol{{Name: "x_name", From: "name"}}}
	}
	run := func(src string, d *spec.Domain, ls []spec.Lookup, nests []spec.Nest) error {
		_, _, err := lookupView(&spec.Spec{Domain: d, Datasource: spec.Datasource{Table: src, Lookups: ls, Nests: nests}}, cat, keys, nil)
		return err
	}
	// 随父: 分库的表 → 随父的表 OK;不分库的表 → 随父的表 NG;随父的表 → 普通的表 NG
	if err := run("orders", dom, []spec.Lookup{look("local", "sku = sku")}, nil); err != nil {
		t.Errorf("分库 → 随父: %v", err)
	}
	if err := run("unsharded", dom, []spec.Lookup{look("local", "sku = sku")}, nil); err == nil || !strings.Contains(err.Error(), "随父") {
		t.Errorf("不分库 → 随父 应该报错: %v", err)
	}
	nestLines := func(ls ...spec.Lookup) []spec.Nest {
		return []spec.Nest{{Name: "ls", Table: "lines", On: []string{"id = order_id"}, OrderBy: []string{"no asc"}, Max: 5,
			Columns: []spec.TakeCol{{Name: "no", From: "no"}}, Lookups: ls}}
	}
	if err := run("orders", dom, nil, nestLines(look("local", "sku = sku"))); err != nil {
		t.Errorf("嵌套(随父)里的关联 → 随父: %v", err)
	}
	if err := run("orders", dom, nil, nestLines(look("plain", "sku = sku"))); err == nil || !strings.Contains(err.Error(), "随父") {
		t.Errorf("随父的子表 → 普通的表 应该报错: %v", err)
	}
	// 对方的列 = 常量 + 按日期: 唯一键 = 键 + 固定的列 + 开始
	free := &spec.Domain{Entities: map[string]spec.Entity{}}
	rate := spec.Lookup{Name: "r", Table: "rates", On: []string{"sku = sku", "kind = 'STD'"}, Columns: []spec.TakeCol{{Name: "rate", From: "rate"}},
		AsOf: &spec.LookupAsOf{Start: "from_day", End: "to_day", EndIn: true, Ref: "day"}}
	if err := run("orders", free, []spec.Lookup{rate}, nil); err != nil {
		t.Errorf("按日期 + 常量: %v", err)
	}
	noConst := rate
	noConst.On = []string{"sku = sku"}
	if err := run("orders", free, []spec.Lookup{noConst}, nil); err == nil || !strings.Contains(err.Error(), "不是主键或唯一约束") {
		t.Errorf("按日期 但 kind 没有固定 应该报错: %v", err)
	}
	badRef := rate
	badRef.AsOf = &spec.LookupAsOf{Start: "from_day", Ref: "nope"}
	if err := run("orders", free, []spec.Lookup{badRef}, nil); err == nil || !strings.Contains(err.Error(), "参照") {
		t.Errorf("参照不是列 应该报错: %v", err)
	}
	onlyConst := rate
	onlyConst.On = []string{"kind = 'STD'"}
	if err := run("orders", free, []spec.Lookup{onlyConst}, nil); err == nil {
		t.Errorf("只有常量的条件 应该报错")
	}
}
