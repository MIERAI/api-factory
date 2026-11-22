package core

// 元件认证: lookup / nest / child_agg 的对方是逻辑表(Source = 来源单元的展开,这里照 JSON版本 的形状手写)。
//
// 物理表是追加型的(每次变更一行: JSON + 操作列 + 登记顺序),逻辑表 = 每个自然键最新的一行,最新的是删除 / 逻辑删除 → 不存在。
// 陷阱: 旧版本的值(按旧版本关联就错)、删除后又登记(复活)、逻辑删除、键为 NULL(取数时丢掉的键 / 可以为空的键自己是一组)、
// JSON 里的 null、大小写不同的键、"10" 和 "9"、两张逻辑表的链式关联(逻辑表 → 逻辑表)、主表本身也是逻辑表。
// SQL 后端: LATERAL + 关联条件下推到取最新之前(pushdown 的等式);参考求值器: 先算整张逻辑表再关联 —— 两边必须一致。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// jsonField 描述: 逻辑列 = JSON 的键(同名),类型
type srcField struct {
	name string
	kind Kind
}

// jsonSource: JSON版本 的来源单元的展开(factory/sources/json_versions.go 的形状): scan → 展开 → 键非空 → 每键最新 → 有效 → 逻辑列。
func jsonSource(table string, fields []srcField, keys, notNull []string) []Step {
	phys := []NamedType{{"id", Type{Kind: Int64}}, {"data", Type{Kind: JSON}}, {"action", Type{Kind: Int32}}, {"reg", Type{Kind: Int64}}}
	var expand, logical []NamedExpr
	for _, f := range fields {
		expand = append(expand, NamedExpr{f.name, JSONField(Col("data"), f.name, Type{Kind: f.kind})})
		logical = append(logical, NamedExpr{f.name, Col(f.name)})
	}
	expand = append(expand, NamedExpr{"_action", Col("action")}, NamedExpr{"_o_reg", Col("reg")}, NamedExpr{"_o_id", Col("id")},
		NamedExpr{"_logical_delete", JSONField(Col("data"), "del", Type{Kind: Text})})
	steps := []Step{Scan(table, phys...), Project(expand...)}
	if len(notNull) > 0 {
		var conj []*Expr
		for _, k := range notNull {
			conj = append(conj, Not(IsNull(Col(k))))
		}
		pred := conj[0]
		if len(conj) > 1 {
			pred = And(conj...)
		}
		steps = append(steps, Filter(pred))
	}
	steps = append(steps,
		PickPerKey(keys, SortKey{"_o_reg", true}, SortKey{"_o_id", true}),
		Filter(And(Cmp("<>", Col("_action"), Const(int64(1), Type{Kind: Int32})),
			Or(IsNull(Col("_logical_delete")), Cmp("<>", Col("_logical_delete"), Const("1", Type{Kind: Text}))))),
		Project(logical...))
	return steps
}

// srcCols: 逻辑表的列(键非空的列不为空,其余可空: 和 sources.Columns 一样)。
func srcCols(fields []srcField, notNull []string) []NamedType {
	var out []NamedType
	for _, f := range fields {
		nn := false
		for _, k := range notNull {
			nn = nn || k == f.name
		}
		out = append(out, NamedType{f.name, Type{Kind: f.kind, Nullable: !nn}})
	}
	return out
}

func TestCertificationSource(t *testing.T) {
	url := os.Getenv("FACTORY_TEST_DB")
	if url == "" {
		t.Skip("FACTORY_TEST_DB 没有设置(元件认证需要 Postgres)")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	schema := fmt.Sprintf("core_cert_src_%d", os.Getpid())
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema,
		"SET search_path = " + schema, "SET TimeZone = 'Asia/Tokyo'",
		`CREATE TABLE pm (k int8 PRIMARY KEY, code text COLLATE "en-US-x-icu", cat text COLLATE "en-US-x-icu")`,
		`CREATE TABLE ev_p (id int8 PRIMARY KEY, data jsonb NOT NULL, action int4 NOT NULL, reg int8 NOT NULL)`,
		`CREATE TABLE ev_l (id int8 PRIMARY KEY, data jsonb NOT NULL, action int4 NOT NULL, reg int8 NOT NULL)`,
		`CREATE TABLE ev_c (id int8 PRIMARY KEY, data jsonb NOT NULL, action int4 NOT NULL, reg int8 NOT NULL)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	env := &certEnv{t: t, ctx: ctx, conn: conn, table: "pm"}
	pmCols := []NamedType{{"k", Type{Kind: Int64}}, {"code", Type{Kind: Text, Nullable: true}}, {"cat", Type{Kind: Text, Nullable: true}}}
	for i, r := range [][2]any{{"a", "x"}, {"A", "y"}, {"B", "x"}, {"c", nil}, {"d", "z"}, {"e", "x"}, {nil, "x"}, {"zz", "y"}, {"10", "x"}, {"9", "q"}, {"a", "y"}, {"", "x"}} {
		k := int64(i + 1)
		if _, err := conn.Exec(ctx, "INSERT INTO pm VALUES ($1, $2, $3)", k, r[0], r[1]); err != nil {
			t.Fatal(err)
		}
		env.rows = append(env.rows, Row{"k": k, "code": r[0], "cat": r[1]})
	}
	id := int64(0)
	ev := func(table string, action int64, data string) {
		id++
		reg := (id*37)%101 + 1000*int64(len(table)) // 登记顺序和 id 的顺序不同(按 reg 取最新)
		if table != "ev_l" {
			reg = id
		}
		if _, err := conn.Exec(ctx, "INSERT INTO "+table+" VALUES ($1, $2::jsonb, $3, $4)", id, data, action, reg); err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(data))
		dec.UseNumber()
		var doc any
		_ = dec.Decode(&doc)
		if env.others == nil {
			env.others = map[string][]Row{}
		}
		env.others[table] = append(env.others[table], Row{"id": id, "data": doc, "action": action, "reg": reg})
	}
	// 商品(键 code): a 有两个版本(旧的 cat = x);B 被删除;c 被逻辑删除;d 删除后又登记(复活);
	// 键为 NULL 的行(丢掉);e 的 name 是 JSON null;A 和 a 是不同的键;"10" / "9";"" 是一个键
	ev("ev_p", 0, `{"code": "a", "name": "A1", "cat": "x", "price": "1.5"}`)
	ev("ev_p", 0, `{"code": "a", "name": "A2", "cat": "y", "price": "1.50"}`)
	ev("ev_p", 0, `{"code": "B", "name": "B1", "cat": "x", "price": 9}`)
	ev("ev_p", 1, `{"code": "B"}`)
	ev("ev_p", 0, `{"code": "c", "name": "C1", "cat": "z", "price": 10}`)
	ev("ev_p", 0, `{"code": "c", "name": "C1", "cat": "z", "price": 10, "del": "1"}`)
	ev("ev_p", 1, `{"code": "d"}`)
	ev("ev_p", 0, `{"code": "d", "name": "D2", "cat": "q", "price": "-2"}`)
	ev("ev_p", 0, `{"code": null, "name": "N", "cat": "x"}`)
	ev("ev_p", 0, `{"name": "N2", "cat": "x"}`)
	ev("ev_p", 0, `{"code": "e", "name": null, "cat": "x"}`)
	ev("ev_p", 0, `{"code": "A", "name": "AA", "cat": "nope"}`)
	ev("ev_p", 0, `{"code": "10", "name": "ten", "cat": "y", "del": "0"}`)
	ev("ev_p", 0, `{"code": "9", "name": "nine", "cat": "x"}`)
	ev("ev_p", 0, `{"code": "", "name": "empty", "cat": "y"}`)
	ev("ev_p", 0, `{"code": "zz", "name": "Z1", "cat": "x"}`)
	ev("ev_p", 0, `{"code": "zz", "name": "Z2", "cat": "x", "del": "1"}`)
	ev("ev_p", 0, `{"code": "zz", "name": "Z3", "cat": "y"}`) // 逻辑删除之后又登记
	// 分类(键 cat): y 有两个版本;z 被删除;q 被逻辑删除;x 正常
	ev("ev_c", 0, `{"cat": "x", "title": "X"}`)
	ev("ev_c", 0, `{"cat": "y", "title": "Y-old"}`)
	ev("ev_c", 0, `{"cat": "y", "title": "Y"}`)
	ev("ev_c", 0, `{"cat": "z", "title": "Z"}`)
	ev("ev_c", 1, `{"cat": "z"}`)
	ev("ev_c", 0, `{"cat": "q", "title": "Q", "del": "1"}`)
	// 标签(键 code + lc,lc 可以为空: 空值自己是一组): 每个商品 0 / 1 / 多个 / 超过上限 2 个
	for _, x := range []struct {
		action int64
		data   string
	}{
		{0, `{"code": "a", "lc": null, "text": "a-def-1"}`}, {0, `{"code": "a", "lc": "x", "text": "a-x"}`},
		{0, `{"code": "a", "lc": null, "text": "a-def-2"}`}, {0, `{"code": "a", "lc": "y", "text": "a-y"}`},
		{1, `{"code": "a", "lc": "y"}`}, // a 的 y 被删除 → a 有 2 个(null, x)
		{0, `{"code": "A", "lc": "x", "text": "A-x"}`},
		{0, `{"code": "d", "lc": "1", "text": "d-1"}`}, {0, `{"code": "d", "lc": "2", "text": "d-2"}`}, {0, `{"code": "d", "lc": "10", "text": "d-10"}`}, // 3 个 > 上限 2
		{0, `{"code": "e", "lc": "x", "text": null}`}, {0, `{"code": "e", "lc": "x", "text": "e-x", "del": "1"}`}, // e 的唯一一个被逻辑删除
		{0, `{"code": null, "lc": "x", "text": "orphan"}`}, {0, `{"lc": "y", "text": "orphan2"}`}, // 键为 NULL: 丢掉
		{0, `{"code": "zz", "lc": "", "text": "zz-empty"}`}, {0, `{"code": "zz", "lc": null, "text": "zz-def"}`},
		{0, `{"code": "9", "lc": "B", "text": "9-B"}`}, {0, `{"code": "9", "lc": "b", "text": "9-b"}`}, {0, `{"code": "9", "lc": "B", "text": "9-B2"}`},
		{0, `{"code": "B", "lc": "x", "text": "B-x"}`}, // B 本身被删除了,标签还在(子表的行跟着父行的关联走)
	} {
		ev("ev_l", x.action, x.data)
	}
	pFields := []srcField{{"code", Text}, {"name", Text}, {"cat", Text}, {"price", Decimal}}
	prod := jsonSource("ev_p", pFields, []string{"code"}, []string{"code"})
	prodCols := srcCols(pFields, []string{"code"})
	cFields := []srcField{{"cat", Text}, {"title", Text}}
	cat := jsonSource("ev_c", cFields, []string{"cat"}, []string{"cat"})
	catCols := srcCols(cFields, []string{"cat"})
	lFields := []srcField{{"code", Text}, {"lc", Text}, {"text", Text}}
	lab := jsonSource("ev_l", lFields, []string{"code", "lc"}, []string{"code"})
	labCols := srcCols(lFields, []string{"code"})
	pmScan := Scan("pm", pmCols...)
	byK := SortKey{Col: "k"}
	lookupP := func(m string) Step {
		st := Lookup("product", prodCols, [][]string{{"code"}}, []JoinKey{{"code", "code"}}, []TakeCol{{"p_name", "name"}, {"p_cat", "cat"}, {"p_price", "price"}}, m)
		st.Source = prod
		return st
	}
	// 1. 普通的表 → 逻辑表(keep / drop)
	for _, m := range []string{"keep", "drop"} {
		env.check("lookup 逻辑表 "+m, &Pipeline{Steps: []Step{pmScan, lookupP(m), Sort(byK)}}, nil)
	}
	// 2. 链式: 普通的表 → 逻辑表(商品)→ 逻辑表(分类,按商品的 cat)
	for _, m1 := range []string{"keep", "drop"} {
		for _, m2 := range []string{"keep", "drop"} {
			lc := Lookup("category", catCols, [][]string{{"cat"}}, []JoinKey{{"p_cat", "cat"}}, []TakeCol{{"c_title", "title"}}, m2)
			lc.Source = cat
			env.check(fmt.Sprintf("lookup 两张逻辑表的链 %s/%s", m1, m2), &Pipeline{Steps: []Step{pmScan, lookupP(m1), lc, Sort(byK)}}, nil)
		}
	}
	// 3. 主表也是逻辑表: 标签(逻辑表)→ 商品(逻辑表),加的列上筛选 / 排序
	labMain := append(append([]Step(nil), lab...), lookupP("keep"))
	lp := &Pipeline{Params: []ParamDecl{{Name: "pn", Type: Type{Kind: Text}}}, Steps: append(append([]Step(nil), labMain...),
		Filter(Or(Absent("pn"), Cmp("=", Col("p_name"), Param("pn")))),
		Sort(SortKey{Col: "code"}, SortKey{Col: "lc"}))}
	for _, v := range []any{"<absent>", "A2", "nine", "A1", "B1"} {
		ps := Params{}
		if v != "<absent>" {
			ps["pn"] = v
		}
		env.check(fmt.Sprintf("逻辑表 → 逻辑表 pn=%v", v), lp, ps)
	}
	// 4. nest: 普通的表 → 逻辑表的子行(按可以为空的键 lc 排序: 空值自己是一组,全序);上限 2(超过 → 多取的一行)
	for _, desc := range []bool{false, true} {
		n := Nest("label", labCols, [][]string{{"code", "lc"}}, []JoinKey{{"code", "code"}}, nil, []SortKey{{"lc", desc}}, 2, "labels", []TakeCol{{"lc", "lc"}, {"text", "text"}})
		n.Source = lab
		env.check(fmt.Sprintf("nest 逻辑表 desc=%v", desc), &Pipeline{Steps: []Step{pmScan, n, Sort(byK)}}, nil)
		nf := n
		nf.Pred = Not(IsNull(Col("text")))
		env.check(fmt.Sprintf("nest 逻辑表 + 子表的条件 desc=%v", desc), &Pipeline{Steps: []Step{pmScan, nf, Sort(byK)}}, nil)
	}
	// 5. child_agg: 件数 / 最大(文本)/ 合计(十进制,另一张逻辑表的价格: 商品按 cat 聚合)
	ca := ChildAgg("label", labCols, []JoinKey{{"code", "code"}}, nil, Count("n"), Agg{Name: "mx", Func: "max", Col: "text"}, Agg{Name: "mn", Func: "min", Col: "lc"})
	ca.Source = lab
	env.check("child_agg 逻辑表", &Pipeline{Steps: []Step{pmScan, ca, Sort(byK)}}, nil)
	cp := ChildAgg("product", prodCols, []JoinKey{{"cat", "cat"}}, nil, Count("n"), Agg{Name: "total", Func: "sum", Col: "price"})
	cp.Source = prod
	env.check("child_agg 逻辑表(非键列关联: 不下推)", &Pipeline{Steps: []Step{pmScan, cp, Sort(byK)}}, nil)
	// 6. 下推真的发生了: 关联条件(键)在取最新之前(DISTINCT ON 的那一层的 WHERE 里)
	s, err := ToSQL(&Pipeline{Steps: []Step{pmScan, lookupP("keep")}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Text, "LEFT JOIN LATERAL") || !strings.Contains(s.Text, "= t1.code") || strings.Index(s.Text, "= t1.code") > strings.Index(s.Text, "ORDER BY") {
		t.Errorf("关联条件没有下推到取最新之前:\n%s", s.Text)
	}
	// 7. DRC: 来源里不能有参数 / 列不一致 / 可以为空的键之外的可空列不能当全序
	bad := lookupP("keep")
	bad.Columns = append([]NamedType(nil), prodCols...)
	bad.Columns[1].Type.Kind = Int32
	if _, err := Check(&Pipeline{Steps: []Step{pmScan, bad}}); err == nil {
		t.Error("列的类型和来源不一致: 应该报错")
	}
	withParam := lookupP("keep")
	withParam.Source = append(append([]Step(nil), prod...), Filter(Cmp("=", Col("cat"), Param("p"))))
	if _, err := Check(&Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Text}}}, Steps: []Step{pmScan, withParam}}); err == nil {
		t.Error("来源引用了参数: 应该报错")
	}
	nn := Nest("label", labCols, [][]string{{"code", "lc"}}, []JoinKey{{"code", "code"}}, nil, []SortKey{{"text", false}}, 2, "labels", []TakeCol{{"text", "text"}})
	nn.Source = lab
	if _, err := Check(&Pipeline{Steps: []Step{pmScan, nn}}); err == nil {
		t.Error("按可空的非键列排序: 应该报错(不是全序)")
	}
	if !t.Failed() {
		t.Logf("逻辑表的关联的元件认证: %d 个用例全部一致", env.n)
	}
}
