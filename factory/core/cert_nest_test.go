package core

// 元件认证: nest(一对多嵌套)和 child_agg(按父行聚合)。
//
// 父表 = 认证的主表 c(k = 1..60)。子表 ch 按 k 的余数造 0 / 1 / 多个 / 正好上限 / 上限 + 1 / 远超上限 个子行,
// 外加关联列为 NULL 的子行、关联不到父行的子行。陷阱:
//   - 十进制: 1.5 和 1.50、0.1 + 0.2、30 位的值(JSON 数字会丢精度;两个后端都必须写成文本)
//   - 时间: 带微秒、日界线前后;认证期间会话时区是 Asia/Tokyo(SQL 后端必须自己换成 UTC,不能依赖会话)
//   - 文本: 大小写 / é / 空字符串(ICU 的排序规则下 COLLATE "C" 才是按字节)
//   - 可以为空的值: sum / min / max 忽略 NULL;全部为 NULL 的组 → sum 0、min / max NULL
//   - 关联列为 NULL(父表的 i 可以为空)、int4 = int8

import (
	"fmt"
	"math/big"
)

const certNestMax = 4

func certNest(env *certEnv, all func(steps ...Step) []Step) {
	t := env.t
	nt := func(name string, k Kind, nullable bool) NamedType {
		return NamedType{name, Type{Kind: k, Nullable: nullable}}
	}
	for _, q := range []string{
		`CREATE TABLE ch (id int8 PRIMARY KEY, ref int8, line int4 NOT NULL, s text COLLATE "en-US-x-icu", n int4,
		   d numeric, ts timestamptz, dt date, ok bool, UNIQUE (ref, line))`,
		`CREATE TABLE chbig (id int8 PRIMARY KEY, ref int8 NOT NULL, v int8 NOT NULL)`,
		"SET TimeZone = 'Asia/Tokyo'", // 子行的时间必须由 SQL 后端换成 UTC(不依赖会话的时区)
	} {
		if _, err := env.conn.Exec(env.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer env.conn.Exec(env.ctx, "SET TimeZone = 'UTC'")
	ch := []NamedType{nt("id", Int64, false), nt("ref", Int64, true), nt("line", Int32, false), nt("s", Text, true),
		nt("n", Int32, true), nt("d", Decimal, true), nt("ts", Timestamp, true), nt("dt", Date, true), nt("ok", Bool, true)}
	sDom := []any{"a", "B", "b", nil, "é", "", "ab"}
	nDom := []any{int64(1), nil, int64(2), int64(10), int64(-3)}
	dDom := []any{rat("1.5"), rat("1.50"), nil, rat("0.1"), rat("0.2"), rat("-2"), rat("123456789012345678901234.5678")}
	tsDom := []any{ts("2026-01-01T00:00:00Z"), ts("2025-12-31T15:00:00Z"), nil, ts("2025-12-31T14:59:59.123456Z"), ts("2026-06-30T23:59:59.5Z")}
	dtDom := []any{day("2026-01-01"), nil, day("2025-12-31"), day("2026-02-28")}
	okDom := []any{true, false, nil}
	// 子行数: k % 8 → 0 / 1 / 3 / 上限 / 上限+1 / 远超上限 / 2 / 0
	counts := []int{0, 1, 3, certNestMax, certNestMax + 1, certNestMax + 3, 2, 0}
	id := int64(0)
	add := func(ref any, line int) {
		id++
		j := int(id)
		env.insertOther("ch", ch, id, ref, int64(line), sDom[j%len(sDom)], nDom[(j/2)%len(nDom)], dDom[(j/3)%len(dDom)],
			tsDom[j%len(tsDom)], dtDom[(j/2)%len(dtDom)], okDom[j%len(okDom)])
	}
	for k := 1; k <= certRows; k++ {
		for l := 1; l <= counts[k%len(counts)]; l++ {
			add(int64(k), (l*7)%23+1) // line: 不按插入顺序
		}
	}
	for l := 1; l <= 3; l++ {
		add(nil, l)        // 关联列为 NULL 的子行: 不属于任何父行
		add(int64(999), l) // 关联不到父行的子行
	}
	for i, v := range []string{"9223372036854775000", "100", "-5"} { // 父行 2: 不溢出;父行 3: 溢出(另外测)
		env.insertOther("chbig", []NamedType{nt("id", Int64, false), nt("ref", Int64, false), nt("v", Int64, false)}, int64(i+1), int64(2), mustInt(v))
	}
	env.insertOther("chbig", []NamedType{nt("id", Int64, false), nt("ref", Int64, false), nt("v", Int64, false)}, int64(10), int64(3), mustInt("9223372036854775000"))
	env.insertOther("chbig", []NamedType{nt("id", Int64, false), nt("ref", Int64, false), nt("v", Int64, false)}, int64(11), int64(3), mustInt("9223372036854775000"))

	unique := [][]string{{"id"}, {"ref", "line"}}
	byK := SortKey{Col: "k"}
	on := []JoinKey{{"k", "ref"}}
	takeAll := []TakeCol{{"line", "line"}, {"s", "s"}, {"n", "n"}, {"d", "d"}, {"ts", "ts"}, {"dt", "dt"}, {"ok", "ok"}}
	nest := func(pred *Expr, order []SortKey, take []TakeCol) Step {
		return Nest("ch", ch, unique, on, pred, order, certNestMax, "items", take)
	}
	head := Project(NamedExpr{"k", Col("k")}, NamedExpr{"i", Col("i")})
	// 1. 顺序: 每一列、每个方向(含 NULL 的列 + 唯一的 line 收尾;文本按字节)
	orders := map[string][]SortKey{
		"line 小到大":         {{Col: "line"}},
		"line 大到小":         {{Col: "line", Desc: true}},
		"s 小到大, line":      {{Col: "s"}, {Col: "line"}},
		"s 大到小, line":      {{Col: "s", Desc: true}, {Col: "line"}},
		"d 大到小, line 大到小":  {{Col: "d", Desc: true}, {Col: "line", Desc: true}},
		"ts 小到大, line 大到小": {{Col: "ts"}, {Col: "line", Desc: true}},
		"dt 大到小, n, line":  {{Col: "dt", Desc: true}, {Col: "n"}, {Col: "line"}},
		"ok, id(另一个唯一键)":   {{Col: "ok"}, {Col: "id"}},
	}
	for name, o := range orders {
		env.check("nest 顺序 "+name, &Pipeline{Steps: all(head, nest(nil, o, takeAll), Sort(byK))}, nil)
	}
	// 2. 子表上的条件(常量 / is_null / 参数;UNKNOWN 丢弃)
	line := []SortKey{{Col: "line"}}
	for name, pred := range map[string]*Expr{
		"n > 1":        Cmp(">", Col("n"), Const(int64(1), Type{Kind: Int32})),
		"s 为空":         IsNull(Col("s")),
		"not ok":       Not(Cmp("=", Col("ok"), Const(true, Type{Kind: Bool}))),
		"d >= 1.5":     Cmp(">=", Col("d"), Const(rat("1.5"), Type{Kind: Decimal})),
		"s < 'b'(按字节)": Cmp("<", Col("s"), Const("b", Type{Kind: Text})),
	} {
		env.check("nest 条件 "+name, &Pipeline{Steps: all(head, nest(pred, line, takeAll), Sort(byK))}, nil)
	}
	for _, v := range []any{"<absent>", int64(1), int64(10), int64(99)} {
		ps := Params{}
		if v != "<absent>" {
			ps["p"] = v
		}
		p := &Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Int32}}},
			Steps: all(head, nest(Or(Absent("p"), Cmp("=", Col("n"), Param("p"))), line, takeAll), Sort(byK))}
		env.check(fmt.Sprintf("nest 条件(参数)p=%v", v), p, ps)
	}
	// 3. 关联: 可以为空的 int4 列(NULL → 没有子行)/ 两列(k 和 line 的组合没有意义,只测多列的写法)
	env.check("nest 关联 i(int4,可空)", &Pipeline{Steps: all(head,
		Nest("ch", ch, unique, []JoinKey{{"i", "ref"}}, nil, line, certNestMax, "items", takeAll), Sort(byK))}, nil)
	env.check("nest 关联两列", &Pipeline{Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"g", Col("g")}),
		Nest("ch", ch, unique, []JoinKey{{"k", "ref"}, {"g", "line"}}, nil, []SortKey{{Col: "id"}}, certNestMax, "items", takeAll), Sort(byK))}, nil)
	// 4. 和别的元件组合: 改名的列、之后筛选父行、排序 + 截取、分页的游标、lookup 之后、nest + child_agg、两个 nest、投影掉
	env.check("nest 改名 + 部分列", &Pipeline{Steps: all(head, nest(nil, line, []TakeCol{{"no", "line"}, {"amount", "d"}}), Sort(byK))}, nil)
	env.check("nest 之后筛选父行 + 排序 + 截取", &Pipeline{Steps: all(
		Filter(Cmp(">", Col("g"), Const(int64(1), Type{Kind: Int32}))), head, nest(nil, line, takeAll),
		Filter(Not(IsNull(Col("i")))), Sort(SortKey{Col: "k", Desc: true}), Limit(7))}, nil)
	for _, after := range []any{"<absent>", int64(20), int64(55)} {
		ps := Params{"row_limit": int64(6)}
		if after != "<absent>" {
			ps["cursor_k"] = after
		}
		p := &Pipeline{Params: []ParamDecl{{Name: "cursor_k", Type: Type{Kind: Int64}}, {Name: "row_limit", Type: Type{Kind: Int64}, Required: true}},
			Steps: all(head, nest(nil, line, takeAll), Filter(TupleAfter([]string{"k"}, []string{"cursor_k"}, false)), Sort(byK), LimitParam("row_limit"))}
		env.check(fmt.Sprintf("nest + 分页 cursor=%v", after), p, ps)
	}
	env.check("nest 在 lookup 之后(用加的列关联)", &Pipeline{Steps: all(
		Lookup("o1", env.otherCols("o1"), [][]string{{"k"}}, []JoinKey{{"i", "k"}}, []TakeCol{{"o_k", "k"}}, "keep"),
		Project(NamedExpr{"k", Col("k")}, NamedExpr{"o_k", Col("o_k")}),
		Nest("ch", ch, unique, []JoinKey{{"o_k", "ref"}}, nil, line, certNestMax, "items", takeAll), Sort(byK))}, nil)
	aggs := []Agg{Count("cnt"), Sum("sum_n", "n"), Sum("sum_d", "d"), Min("min_s", "s"), Max("max_s", "s"),
		Min("min_ts", "ts"), Max("max_dt", "dt"), Min("min_d", "d"), Max("max_n", "n"), Sum("sum_line", "line")}
	env.check("nest + child_agg + 两个 nest", &Pipeline{Steps: all(head,
		nest(nil, line, []TakeCol{{"line", "line"}}),
		ChildAgg("ch", ch, on, nil, aggs...),
		Nest("ch", ch, unique, on, IsNull(Col("s")), []SortKey{{Col: "line", Desc: true}}, 2, "nulls", []TakeCol{{"id", "id"}}),
		Sort(byK))}, nil)
	env.check("nest 之后投影掉子行", &Pipeline{Steps: all(head, nest(nil, line, takeAll), Project(NamedExpr{"k", Col("k")}), Sort(byK))}, nil)
	env.check("nest 之后 is_null(子行的列表)", &Pipeline{Steps: all(head, nest(nil, line, takeAll), Filter(Not(IsNull(Col("items")))), Sort(byK))}, nil)
	// 日期 / 时间的写法不受会话的 DateStyle 影响(这一项只输出 k 和子行: 父行的日期不经过 ::text)
	if _, err := env.conn.Exec(env.ctx, "SET DateStyle = 'German'"); err != nil {
		t.Fatal(err)
	}
	env.check("nest DateStyle = German", &Pipeline{Steps: all(Project(NamedExpr{"k", Col("k")}), nest(nil, line, takeAll), Sort(byK))}, nil)
	if _, err := env.conn.Exec(env.ctx, "SET DateStyle = 'ISO'"); err != nil {
		t.Fatal(err)
	}
	env.check("nest 上限 1", &Pipeline{Steps: all(head, Nest("ch", ch, unique, on, nil, line, 1, "items", takeAll), Sort(byK))}, nil)

	// 5. child_agg: 空集合 / NULL 值 / 十进制精度 / 文本按字节;带条件;可空的关联列
	env.check("child_agg 全部的聚合", &Pipeline{Steps: all(head, ChildAgg("ch", ch, on, nil, aggs...), Sort(byK))}, nil)
	env.check("child_agg 带条件(ok)", &Pipeline{Steps: all(head, ChildAgg("ch", ch, on, Cmp("=", Col("ok"), Const(true, Type{Kind: Bool})), aggs...), Sort(byK))}, nil)
	env.check("child_agg 带条件(全部为 NULL 的组: s 为空)", &Pipeline{Steps: all(head, ChildAgg("ch", ch, on, IsNull(Col("s")), aggs...), Sort(byK))}, nil)
	env.check("child_agg 关联 i(可空)", &Pipeline{Steps: all(head, ChildAgg("ch", ch, []JoinKey{{"i", "ref"}}, nil, aggs...), Sort(byK))}, nil)
	env.check("child_agg 之后筛选 / 排序聚合的列", &Pipeline{Steps: all(head, ChildAgg("ch", ch, on, nil, Count("cnt"), Sum("sum_d", "d"), Max("max_s", "s")),
		Filter(Cmp(">", Col("cnt"), Const(int64(1), Type{Kind: Int64}))), Sort(SortKey{Col: "sum_d", Desc: true}, SortKey{Col: "max_s"}, byK), Limit(12))}, nil)
	big := []NamedType{nt("id", Int64, false), nt("ref", Int64, false), nt("v", Int64, false)}
	env.check("child_agg int8 的和(接近上限,不溢出)", &Pipeline{Steps: all(head, Filter(Cmp("<", Col("k"), Const(int64(3), Type{Kind: Int64}))),
		ChildAgg("chbig", big, on, nil, Sum("s", "v"), Count("c")), Sort(byK))}, nil)
	env.checkError("child_agg int8 的和溢出 → 两边都报错", &Pipeline{Steps: all(head, ChildAgg("chbig", big, on, nil, Sum("s", "v")), Sort(byK))})

	// 6. 类型: rows 的字段 / child_agg 的可空性(集合可能为空: min / max 可空,count / sum 不可空)
	s, err := Check(&Pipeline{Steps: all(head, nest(nil, line, takeAll), ChildAgg("ch", ch, on, nil, Count("c"), Sum("sd", "d"), Min("ml", "line")))})
	if err != nil {
		t.Fatal(err)
	}
	if ty, _ := s.Col("items"); ty.Kind != Rows || len(ty.Fields) != len(takeAll) || ty.Nullable {
		t.Errorf("nest: items 的类型 %+v", ty)
	}
	for col, want := range map[string]Type{"c": {Kind: Int64}, "sd": {Kind: Decimal}, "ml": {Kind: Int32, Nullable: true}} {
		if ty, _ := s.Col(col); ty.Kind != want.Kind || ty.Nullable != want.Nullable {
			t.Errorf("child_agg %s: 类型 %+v,应该是 %+v", col, ty, want)
		}
	}
	// 7. DRC: 顺序不是全序 / 唯一键里有可空的列 / 子行的列是 json / 重名 / 上限 / 聚合的列
	chj := append(append([]NamedType(nil), ch...), nt("j", JSON, true))
	for name, st := range map[string]Step{
		"顺序不是全序":     nest(nil, []SortKey{{Col: "s"}}, takeAll),
		"唯一键有可空的列":   Nest("ch", ch, [][]string{{"s", "line"}}, on, nil, []SortKey{{Col: "s"}, {Col: "line"}}, 4, "items", takeAll),
		"子行的列是 json": Nest("ch", chj, unique, on, nil, line, 4, "items", []TakeCol{{"j", "j"}}),
		"重名":         Nest("ch", ch, unique, on, nil, line, 4, "i", takeAll),
		"上限 0":       Nest("ch", ch, unique, on, nil, line, 0, "items", takeAll),
		"按 rows 排序":  nest(nil, []SortKey{{Col: "line"}}, takeAll),
		"关联的列不存在":    Nest("ch", ch, unique, []JoinKey{{"nope", "ref"}}, nil, line, 4, "items", takeAll),
		"条件不是 bool":  Nest("ch", ch, unique, on, Col("n"), line, 4, "items", takeAll),
		"sum 文本":     ChildAgg("ch", ch, on, nil, Sum("x", "s")),
		"聚合的列不存在":    ChildAgg("ch", ch, on, nil, Max("x", "nope")),
		"聚合重名":       ChildAgg("ch", ch, on, nil, Count("k")),
		"count 带列":   ChildAgg("ch", ch, on, nil, Agg{Name: "x", Func: "count", Col: "n"}),
	} {
		steps := all(head, st)
		if name == "按 rows 排序" {
			steps = append(steps, Sort(SortKey{Col: "items"}))
		}
		if _, err := Check(&Pipeline{Steps: steps}); err == nil {
			t.Errorf("nest / child_agg DRC %s: 应该报错", name)
		}
	}
	// 8. 参考求值器: 超过上限的判定(Over)和 SQL 多取的一行一致: 子行数 = 上限 → 不超;上限 + 1 → 超
	res, err := Eval(&Pipeline{Steps: all(head, nest(nil, line, takeAll), Sort(byK))}, env.tables(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		k := int(r["k"].(int64))
		if k > certRows { // 之后的认证插入的行(没有子行)
			continue
		}
		c := r["items"].(Children)
		want := counts[k%len(counts)]
		if len(c.Rows) != min(want, certNestMax+1) || c.Over() != (want > certNestMax) {
			t.Errorf("nest: k=%d 子行 %d 个(Over=%v),应该是 %d 个(上限 %d)", k, len(c.Rows), c.Over(), want, certNestMax)
		}
	}
}

func mustInt(s string) int64 {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok || !n.IsInt64() {
		panic(s)
	}
	return n.Int64()
}

// otherCols: certLookup 建的表的列(insertOther 记下的第一行的键不够: 用 Check 不到的地方直接写)。
func (e *certEnv) otherCols(table string) []NamedType {
	switch table {
	case "o1":
		return []NamedType{{"k", Type{Kind: Int64}}, {"name", Type{Kind: Text}}, {"ref", Type{Kind: Text, Nullable: true}}, {"amt", Type{Kind: Decimal, Nullable: true}}}
	}
	panic(table)
}
