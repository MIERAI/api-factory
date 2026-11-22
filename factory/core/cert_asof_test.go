package core

// 元件认证: lookup 的扩展(lookup_asof.go): 按日期查历史表(AsOf)和对方的条件(Pred)。
//
// 主表 c: g = 1..3,dt ∈ {NULL, 2025-12-31, 2026-01-01, 2026-01-02}(参照)。历史表 h: (g, kind, start) 唯一。
// 陷阱: 开始 = 参照(含)/ 结束 = 参照(不含)/ 结束为空(无期限)/ 开始最大的那一行已经结束(不退回更早的行:
// 先按 开始 ≤ 参照 且 参照 < 结束 筛,再取开始最大的)/ kind 不同的行(对方的条件)/ 参照为空 / 键为空。

import (
	"fmt"
	"strings"
)

func certAsOf(env *certEnv, all func(steps ...Step) []Step) {
	t := env.t
	nt := func(name string, k Kind, nullable bool) NamedType {
		return NamedType{name, Type{Kind: k, Nullable: nullable}}
	}
	if _, err := env.conn.Exec(env.ctx, `CREATE TABLE h (g int4, kind text COLLATE "en-US-x-icu" NOT NULL, start date NOT NULL,
		fin date, v text COLLATE "en-US-x-icu", UNIQUE (g, kind, start))`); err != nil {
		t.Fatal(err)
	}
	h := []NamedType{nt("g", Int32, true), nt("kind", Text, false), nt("start", Date, false), nt("fin", Date, true), nt("v", Text, true)}
	for _, r := range [][]any{
		{int64(1), "A", day("2025-12-01"), nil, "1A-old"},
		{int64(1), "A", day("2026-01-01"), day("2026-01-02"), "1A-0101"}, // 开始 = 参照 / 结束 = 参照(01-02 时无效)
		{int64(1), "B", day("2025-12-31"), nil, "1B"},
		{int64(2), "A", day("2025-12-31"), day("2026-01-01"), "2A-1231"}, // 01-01 时已经结束 → 01-01 没有(不退回)
		{int64(2), "A", day("2025-01-01"), nil, "2A-old"},                // 开始更早、无期限: 01-01 时匹配这一行
		{int64(3), "A", day("2026-01-03"), nil, "3A-future"},
		{nil, "A", day("2020-01-01"), nil, "null-key"},
	} {
		env.insertOther("h", h, r...)
	}
	kindA := Cmp("=", Col("kind"), Const("A", Type{Kind: Text}))
	head := Project(NamedExpr{"k", Col("k")}, NamedExpr{"g", Col("g")}, NamedExpr{"dt", Col("dt")})
	byK := SortKey{Col: "k"}
	for _, m := range []string{"keep", "drop"} {
		for _, end := range []string{"fin", "fin(含)", ""} {
			st := Lookup("h", h, [][]string{{"g", "kind", "start"}}, []JoinKey{{"g", "g"}}, []TakeCol{{"hv", "v"}, {"hstart", "start"}}, m)
			st.Pred = kindA
			st.AsOf = &AsOf{Start: "start", End: strings.TrimSuffix(end, "(含)"), EndIn: strings.HasSuffix(end, "(含)"), Ref: Col("dt")}
			env.check("lookup 按日期("+m+", 结束="+end+")", &Pipeline{Steps: all(head, st, Sort(byK))}, nil)
		}
	}
	// 参照 = 今天(local_date(now()));对方的条件之外的键(kind 也在条件里: 两列的键)
	for _, now := range []string{"2025-12-31T15:00:00Z", "2026-01-01T15:00:00Z"} {
		st := Lookup("h", h, [][]string{{"g", "kind", "start"}}, []JoinKey{{"g", "g"}}, []TakeCol{{"hv", "v"}}, "keep")
		st.Pred = kindA
		st.AsOf = &AsOf{Start: "start", End: "fin", Ref: Today("Asia/Tokyo")}
		env.check("lookup 按日期(参照 = 今天 "+now+")", &Pipeline{Steps: all(head, st, Sort(byK))}, Params{NowParam: ts(now)})
	}
	// 对方的条件(没有按日期): 固定的列 + 键 覆盖唯一键
	st := Lookup("h", h, [][]string{{"g", "kind", "start"}}, []JoinKey{{"g", "g"}}, []TakeCol{{"hv", "v"}}, "keep")
	st.Pred = And(kindA, Cmp("=", Col("start"), Const(day("2025-12-31"), Type{Kind: Date})))
	env.check("lookup 对方的条件(kind = A 并且 start = 2025-12-31)", &Pipeline{Steps: all(head, st, Sort(byK))}, nil)
	// 整数编码的日期(YYYYMMDD)的开始 / 结束列(AsOf.YMD): 不合法的开始(20251232 / 20260230 / 0)/ 不合法的结束(20261301)
	// 的行永远不匹配(不合法的结束不是"无期限");合法的值的整数顺序 = 日期的顺序。参照为空 → 不匹配
	if _, err := env.conn.Exec(env.ctx, `CREATE TABLE hy (g int4, kind text COLLATE "en-US-x-icu" NOT NULL, start int4 NOT NULL,
		fin int4, v text COLLATE "en-US-x-icu", UNIQUE (g, kind, start))`); err != nil {
		t.Fatal(err)
	}
	hy := []NamedType{nt("g", Int32, true), nt("kind", Text, false), nt("start", Int32, false), nt("fin", Int32, true), nt("v", Text, true)}
	for _, r := range [][]any{
		{int64(1), "A", int64(20251201), nil, "1A-old"},
		{int64(1), "A", int64(20251232), nil, "1A-bad-start"},           // 不合法的开始: 整数上在 1231 和 0101 之间
		{int64(1), "A", int64(20260101), int64(20260101), "1A-0101"},    // 开始 = 结束 = 参照(含)
		{int64(2), "A", int64(20251231), int64(20261301), "2A-bad-fin"}, // 不合法的结束: 不匹配(不退回无期限)
		{int64(2), "A", int64(20250101), nil, "2A-old"},                 //
		{int64(3), "A", int64(20260230), nil, "3A-bad"},                 // 不合法: 2 月 30 日
		{int64(3), "A", int64(20251230), int64(20260101), "3A"},         // 01-02 时已经结束
		{int64(3), "A", int64(0), nil, "3A-zero"},                       // 0: 不合法
		{int64(3), "B", int64(20251231), nil, "3B"},                     // kind 不同
		{int64(2), "A", int64(20260102), int64(20991231), "2A-0102"},    //
	} {
		env.insertOther("hy", hy, r...)
	}
	for _, m := range []string{"keep", "drop"} {
		for _, end := range []string{"fin", "fin(含)", ""} {
			st := Lookup("hy", hy, [][]string{{"g", "kind", "start"}}, []JoinKey{{"g", "g"}}, []TakeCol{{"hv", "v"}, {"hstart", "start"}}, m)
			st.Pred = kindA
			st.AsOf = &AsOf{Start: "start", End: strings.TrimSuffix(end, "(含)"), EndIn: strings.HasSuffix(end, "(含)"), Ref: Col("dt"), YMD: true}
			env.check("lookup 按日期 YYYYMMDD("+m+", 结束="+end+")", &Pipeline{Steps: all(head, st, Sort(byK))}, nil)
		}
	}
	for _, now := range []string{"2025-12-31T15:00:00Z", "2026-01-01T14:59:59Z", "2026-01-01T15:00:00Z"} {
		st := Lookup("hy", hy, [][]string{{"g", "kind", "start"}}, []JoinKey{{"g", "g"}}, []TakeCol{{"hv", "v"}}, "keep")
		st.Pred = kindA
		st.AsOf = &AsOf{Start: "start", End: "fin", EndIn: true, Ref: Today("Asia/Tokyo"), YMD: true}
		env.check("lookup 按日期 YYYYMMDD(参照 = 今天 "+now+")", &Pipeline{Steps: all(head, st, Sort(byK))}, Params{NowParam: ts(now)})
	}
	// 错误实现(ymd_raw: 不检查合法性)在这份数据上和正确的结果不同(敏感度)
	{
		st := Lookup("hy", hy, [][]string{{"g", "kind", "start"}}, []JoinKey{{"g", "g"}}, []TakeCol{{"hv", "v"}}, "keep")
		st.Pred = kindA
		st.AsOf = &AsOf{Start: "start", End: "fin", EndIn: true, Ref: Col("dt"), YMD: true}
		good, err := Eval(&Pipeline{Steps: all(head, st, Sort(byK))}, env.tables(), nil)
		if err != nil {
			t.Fatal(err)
		}
		bad := st
		a := *st.AsOf
		a.Mutant = "ymd_raw"
		bad.AsOf = &a
		mut, err := Eval(&Pipeline{Steps: all(head, bad, Sort(byK))}, env.tables(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(good.Rows) == fmt.Sprint(mut.Rows) {
			t.Error("按日期 YYYYMMDD: ymd_raw 的错误实现和正确的结果一样(数据没有落到不合法的值上)")
		}
	}
	// DRC: 没有固定 kind → 不唯一;开始列和参照的类型不同;条件引用参数
	for name, f := range map[string]func(*Step){
		"按日期但不唯一(kind 没有固定)": func(s *Step) { s.AsOf = &AsOf{Start: "start", Ref: Col("dt")} },
		"开始列和参照的类型不同":        func(s *Step) { s.Pred = kindA; s.AsOf = &AsOf{Start: "start", Ref: Col("k")} },
		"对方的条件引用参数":          func(s *Step) { s.Pred = And(kindA, Cmp("=", Col("v"), Param("p"))) },
		"没有结束列":              func(s *Step) { s.Pred = kindA; s.AsOf = &AsOf{Start: "start", End: "nope", Ref: Col("dt")} },
		"YMD 但开始列是日期":        func(s *Step) { s.Pred = kindA; s.AsOf = &AsOf{Start: "start", Ref: Col("dt"), YMD: true} },
	} {
		s := Lookup("h", h, [][]string{{"g", "kind", "start"}}, []JoinKey{{"g", "g"}}, []TakeCol{{"hv", "v"}}, "keep")
		f(&s)
		if _, err := Check(&Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Text}}}, Steps: all(head, s)}); err == nil {
			t.Errorf("lookup 扩展 DRC %s: 应该报错", name)
		}
	}
}
