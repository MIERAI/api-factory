package core

// 元件认证: group / rank。值域的陷阱: 空集合、NULL 的键(一组,不是每个 NULL 一组)、NULL 的值(sum / min / max 忽略)、
// 全部为 NULL(sum 0、min / max NULL)、十进制求和的精度(0.1 + 0.2、20 位以上的有效数字)、
// 文本的按字节(min(t) 在 ICU 下是 "a",按字节是 "A")、整数求和溢出(查询失败)、名次的并列和跳号。

import (
	"fmt"
	"math/big"
)

func certGroup(env *certEnv, all func(steps ...Step) []Step) {
	t := env.t
	byKey := func(keys ...string) Step {
		var o []SortKey
		for _, k := range keys {
			o = append(o, SortKey{Col: k})
		}
		return Sort(o...)
	}
	// a. 每种键(不可空 / 可空的整数 / 文本 / 十进制 / 日期 / 时间;单列和两列)× 全部聚合
	aggs := []Agg{Count("n"), Sum("si", "i"), Sum("sd", "d"), Min("mi", "i"), Max("mi2", "i"), Min("md", "d"), Max("xd", "d"),
		Min("mt", "t"), Max("xt", "t"), Min("ms", "s"), Max("xs", "s"), Min("mdt", "dt"), Max("xts", "ts"), Sum("sg", "g"), Sum("sk", "k")}
	for _, keys := range [][]string{{"g"}, {"t"}, {"i"}, {"s"}, {"d"}, {"b"}, {"dt"}, {"ts"}, {"g", "s"}, {"s", "i"}, {"b", "g"}} {
		env.check(fmt.Sprintf("group %v 全部聚合", keys), &Pipeline{Steps: all(Group(keys, aggs...), byKey(keys...))}, nil)
		env.check(fmt.Sprintf("group %v 无序", keys), &Pipeline{Steps: all(Group(keys, Count("n"), Sum("sd", "d")))}, nil)
	}
	// b. 空集合: 没有输入行 → 没有输出行(不是一行合计 0)
	none := Filter(Cmp("<", Col("k"), Const(int64(0), Type{Kind: Int64})))
	env.check("group 空集合", &Pipeline{Steps: all(none, Group([]string{"g"}, Count("n"), Sum("sd", "d"), Min("md", "d")), byKey("g"))}, nil)
	// c. 全部为 NULL 的组: sum → 0,min / max → NULL(i 为 NULL 的行按 g 分组)
	env.check("group 全部为 NULL 的值", &Pipeline{Steps: all(Filter(IsNull(Col("i"))), Group([]string{"g"}, Sum("si", "i"), Min("mi", "i"), Max("xi", "i"), Count("n")), byKey("g"))}, nil)
	// d. 筛选 → 分组 → 投影 → 排序 → 截取(前N条 的形状);参数筛选在分组之前
	pt := &Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Int32}}}, Steps: all(
		Filter(Or(Absent("p"), Cmp("=", Col("g"), Param("p")))),
		Group([]string{"t"}, Sum("total", "d"), Count("n")),
		Project(NamedExpr{"t", Col("t")}, NamedExpr{"total", Col("total")}, NamedExpr{"n", Col("n")}),
		Sort(SortKey{"total", true}, SortKey{"t", false}), Limit(3))}
	for _, v := range []any{"<absent>", int64(1), int64(2), int64(3), int64(9)} {
		ps := Params{}
		if v != "<absent>" {
			ps["p"] = v
		}
		env.check(fmt.Sprintf("group + 前N条 p=%v", v), pt, ps)
	}
	// e. 分组之后的筛选(子查询)+ 游标(分页的形状: 排序 = 全部分组键)
	for _, desc := range []bool{false, true} {
		pg := &Pipeline{Params: []ParamDecl{{Name: "cg", Type: Type{Kind: Int32}}, {Name: "cs", Type: Type{Kind: Text}}}, Steps: all(
			Filter(Not(IsNull(Col("s")))),
			Group([]string{"g", "s"}, Count("n")),
			Project(NamedExpr{"g", Col("g")}, NamedExpr{"s", Col("s")}, NamedExpr{"n", Col("n")}),
			Filter(TupleAfter([]string{"g", "s"}, []string{"cg", "cs"}, desc)),
			Sort(SortKey{"g", desc}, SortKey{"s", desc}), Limit(4))}
		for _, ps := range []Params{{}, {"cg": int64(1), "cs": "a"}, {"cg": int64(2), "cs": "B"}, {"cg": int64(2), "cs": "é"}, {"cg": int64(3), "cs": ""}} {
			env.check(fmt.Sprintf("group + 游标 desc=%v %v", desc, ps), pg, ps)
		}
	}
	// f. rank: 并列同名次、下一个名次跳过。按件数(并列多)、按可空的值(NULL 的位置)、按文本(按字节)、两列
	for _, order := range [][]SortKey{
		{{"n", true}}, {{"n", false}}, {{"mi", false}}, {{"mi", true}}, {{"mt", false}}, {{"mt", true}},
		{{"n", true}, {"mi", false}}, {{"sd", true}}, {{"ms", false}},
	} {
		p := &Pipeline{Steps: all(Group([]string{"g", "b"}, Count("n"), Min("mi", "i"), Min("mt", "t"), Sum("sd", "d"), Min("ms", "s")),
			Rank("r", order...), Sort(SortKey{"r", false}, SortKey{"g", false}, SortKey{"b", false}))}
		env.check(fmt.Sprintf("rank %v", order), p, nil)
		p = &Pipeline{Steps: all(Group([]string{"s"}, Count("n"), Min("mi", "i"), Min("mt", "t"), Sum("sd", "d"), Min("ms", "s")),
			Rank("r", order...), Project(NamedExpr{"s", Col("s")}, NamedExpr{"r", Col("r")}, NamedExpr{"n", Col("n")}),
			Sort(SortKey{"r", false}, SortKey{"s", false}), Limit(5))}
		env.check(fmt.Sprintf("rank 可空的键 + 前N条 %v", order), p, nil)
	}
	// 并列和跳号必须真的出现(否则 rank / dense_rank / row_number 区分不开)
	if res, err := Eval(&Pipeline{Steps: all(Group([]string{"g", "b"}, Count("n")), Rank("r", SortKey{"n", true}))}, env.tables(), nil); err == nil {
		ties, skip := false, false
		seen := map[int64]int{}
		for _, r := range res.Rows {
			seen[r["r"].(int64)]++
		}
		for k, c := range seen {
			ties = ties || c > 1
			if c > 1 && seen[k+1] == 0 && len(seen) > 1 {
				skip = true
			}
		}
		if !ties || !skip {
			t.Errorf("rank 的认证数据没有并列 / 跳号: %v", seen)
		}
	}
	// g. 另外的表: 十进制的精度、整数求和溢出、文本键的按字节分组(大小写 / 组合字符)
	if _, err := env.conn.Exec(env.ctx, `CREATE TABLE gq (k int8 PRIMARY KEY, g text COLLATE "en-US-x-icu", d numeric, n int8, m int4)`); err != nil {
		t.Fatal(err)
	}
	gqCols := []NamedType{{"k", Type{Kind: Int64}}, {"g", Type{Kind: Text, Nullable: true}}, {"d", Type{Kind: Decimal, Nullable: true}},
		{"n", Type{Kind: Int64, Nullable: true}}, {"m", Type{Kind: Int32, Nullable: true}}}
	huge := "123456789012345678901234567890.123456789"
	gq := []Row{
		{"k": int64(1), "g": "a", "d": rat("0.1"), "n": int64(9223372036854775807), "m": int64(2147483647)},
		{"k": int64(2), "g": "a", "d": rat("0.2"), "n": int64(1), "m": int64(2147483647)},
		{"k": int64(3), "g": "A", "d": rat(huge), "n": int64(-5), "m": int64(1)},
		{"k": int64(4), "g": "A", "d": rat("0.000000000000000001"), "n": nil, "m": nil},
		{"k": int64(5), "g": "é", "d": rat("-0.30"), "n": int64(2), "m": int64(-2147483648)},
		{"k": int64(6), "g": "é", "d": rat("1.10"), "n": int64(3), "m": nil},
		{"k": int64(7), "g": nil, "d": nil, "n": nil, "m": nil},
		{"k": int64(8), "g": nil, "d": rat("2.5"), "n": int64(4), "m": int64(5)},
		{"k": int64(9), "g": "b", "d": nil, "n": nil, "m": nil},
	}
	for _, r := range gq {
		var d any
		if x, ok := r["d"].(*big.Rat); ok {
			d = RatString(x)
		}
		if _, err := env.conn.Exec(env.ctx, `INSERT INTO gq VALUES ($1, $2, $3::numeric, $4, $5)`, r["k"], r["g"], d, r["n"], r["m"]); err != nil {
			t.Fatal(err)
		}
	}
	env.extra = map[string][]Row{"gq": gq}
	scanGQ := Scan("gq", gqCols...)
	env.check("group 十进制精度 / 按字节的文本键", &Pipeline{Steps: []Step{scanGQ, Group([]string{"g"}, Sum("sd", "d"), Count("c"), Min("md", "d"), Max("xd", "d"), Sum("sm", "m")), Sort(SortKey{Col: "g"})}}, nil)
	env.check("group 十进制精度(不排序)", &Pipeline{Steps: []Step{scanGQ, Filter(Cmp("<>", Col("k"), Const(int64(1), Type{Kind: Int64}))), Group([]string{"m"}, Sum("sd", "d"), Sum("sn", "n"))}}, nil)
	env.checkError("group 整数求和溢出(int8)", &Pipeline{Steps: []Step{scanGQ, Filter(Cmp("<=", Col("k"), Const(int64(2), Type{Kind: Int64}))), Group([]string{"g"}, Sum("sn", "n"))}})
	// int4 的和超过 int4 不算溢出(结果是 int64)
	env.check("group int4 求和超过 int4", &Pipeline{Steps: []Step{scanGQ, Group([]string{"g"}, Sum("sm", "m")), Sort(SortKey{Col: "g"})}}, nil)
	env.extra = nil
}
