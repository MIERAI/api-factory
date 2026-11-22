package core

// ymd(整数编码的日期 YYYYMMDD)的元件认证: Postgres 上 SQL 后端的结果 vs 参考求值器,逐值比较。
// 值域: 合法的日期(月末、闰年 2/29、世纪的闰年、1000-01-01 / 9999-12-31)+ 不合法的整数(2/30、4/31、2/29 的平年、
// 月 0 / 13、日 0 / 32、0、负数、7 位 / 9 位、NULL);int4 和 int8 两种列。
// 用法: 值(project: CASE 的换算)/ 条件的肯定位置(filter: 改写成整数列上的比较 + 合法性)/ 否定位置(not 之下: 不改写)/
// 派生参数的形状(月初(ymd(参数) - 1 个月) ≤ ymd(列) ≤ 月初(ymd(参数)) - 1 天;参数也可能不合法 → NULL → 不命中)。

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var ymdInts = []any{nil, int64(20260131), int64(20260201), int64(20260228), int64(20260301), int64(20240229), int64(20250228),
	int64(20250229), int64(20260230), int64(20260431), int64(20260100), int64(20261301), int64(20260001), int64(20261232),
	int64(20001231), int64(20000229), int64(19000229), int64(10000101), int64(99991231), int64(9991231), int64(100000101),
	int64(0), int64(-20260101), int64(20260315), int64(20260331), int64(20251231), int64(20260101), int64(20260229)}

func TestCertificationYMD(t *testing.T) {
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
	schema := fmt.Sprintf("core_cert_ymd_%d", os.Getpid())
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema,
		"SET search_path = " + schema, "SET TimeZone = 'America/Los_Angeles'",
		`CREATE TABLE ym (k int8 PRIMARY KEY, n int4, m int8)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	env := &certEnv{t: t, ctx: ctx, conn: conn, table: "ym"}
	cols := []NamedType{{"k", Type{Kind: Int64}}, {"n", Type{Kind: Int32, Nullable: true}}, {"m", Type{Kind: Int64, Nullable: true}}}
	for i, v := range ymdInts {
		var m any = v
		if v != nil && i%3 == 0 {
			m = v.(int64) + 1 // int8 的列: 值稍微错开(不合法 / 合法的组合不同)
		}
		k := int64(i + 1)
		if _, err := conn.Exec(ctx, "INSERT INTO ym VALUES ($1, $2::int4, $3::int8)", k, v, m); err != nil {
			t.Fatal(err)
		}
		env.rows = append(env.rows, Row{"k": k, "n": v, "m": m})
	}
	scan := Scan("ym", cols...)
	byK := SortKey{Col: "k"}
	all := func(steps ...Step) []Step { return append([]Step{scan}, steps...) }
	dateP := []any{"<absent>", day("2026-02-01"), day("2026-02-28"), day("2026-03-01"), day("2024-02-29"), day("2000-02-29"),
		day("1000-01-01"), day("9999-12-31"), day("0999-12-31"), day("2026-01-31")}
	// 1. 值: ymd(列) / 月初 / 加一个月(CASE 的换算;不合法 → NULL)
	for _, c := range []string{"n", "m"} {
		env.check("ymd 值 "+c, &Pipeline{Steps: all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", YMD(Col(c))},
			NamedExpr{"mo", Trunc(YMD(Col(c)), Month, "")}, NamedExpr{"nx", Add(YMD(Col(c)), Const(int64(1), Type{Kind: Int32}), Month, true, "")}), Sort(byK))}, nil)
	}
	// 2. 条件: 肯定位置(改写成整数的比较)/ 值的位置(project 里的比较)/ 否定位置(not 之下)/ 不传 = 不筛选
	for _, c := range []string{"n", "m"} {
		for op := range cmpOps {
			p := &Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Date}}}}
			for _, v := range dateP {
				ps := Params{}
				if v != "<absent>" {
					ps["p"] = v
				}
				x := Cmp(op, YMD(Col(c)), Param("p"))
				p.Steps = all(Filter(x), Sort(byK))
				env.check(fmt.Sprintf("ymd filter %s %s %v", c, op, v), p, ps)
				p.Steps = all(Filter(Or(Absent("p"), x)), Sort(byK))
				env.check(fmt.Sprintf("ymd 筛选单元 %s %s %v", c, op, v), p, ps)
				p.Steps = all(Filter(Not(x)), Sort(byK))
				env.check(fmt.Sprintf("ymd not %s %s %v", c, op, v), p, ps)
				p.Steps = all(Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", x}), Sort(byK))
				env.check(fmt.Sprintf("ymd project %s %s %v", c, op, v), p, ps)
			}
		}
	}
	// 3. 派生参数的形状: 参数也是整数编码(可能不合法 → NULL → 不命中)
	q := &Pipeline{Params: []ParamDecl{{Name: "base", Type: Type{Kind: Int32}, Required: true}}, Steps: all(
		Filter(Cmp(">=", YMD(Col("n")), Trunc(Add(YMD(Param("base")), Const(int64(1), Type{Kind: Int32}), Month, true, ""), Month, ""))),
		Filter(Cmp("<=", YMD(Col("n")), Add(Trunc(YMD(Param("base")), Month, ""), Const(int64(1), Type{Kind: Int32}), Day, true, ""))),
		Sort(byK))}
	for _, b := range ymdInts[1:] {
		env.check(fmt.Sprintf("ymd 上个月 base=%v", b), q, Params{"base": b})
	}
	// 4. 参考实现本身: 合法 ↔ Go 的日历(每一天往返)
	for d := day("1999-12-25"); d.Before(day("2001-03-05")); d = d.AddDate(0, 0, 1) {
		if got := YMDDate(YMDInt(d), false); got != d {
			t.Fatalf("YMDDate(%d) = %v", YMDInt(d), got)
		}
	}
	if YMDDate(20260230, true) != time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) || YMDDate(20260230, false) != nil {
		t.Fatal("YMDDate loose / strict")
	}
	// 5. 改写在 SQL 里真的发生了(整数列上的比较,索引能用);not 之下没有改写
	s, err := ToSQL(&Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Date}}}, Steps: all(Filter(Cmp(">=", YMD(Col("n")), Param("p"))))}, Options{})
	if err != nil || !containsAll(s.Text, "(t1.n >= (to_char($1::date, 'YYYYMMDD'))::int4)") {
		t.Errorf("肯定位置没有改写: %v\n%s", err, s.Text)
	}
	s, _ = ToSQL(&Pipeline{Params: []ParamDecl{{Name: "p", Type: Type{Kind: Date}}}, Steps: all(Filter(Not(Cmp(">=", YMD(Col("n")), Param("p")))))}, Options{})
	if containsAll(s.Text, "to_char(") {
		t.Errorf("not 之下不能改写:\n%s", s.Text)
	}
	if !t.Failed() {
		t.Logf("ymd 的元件认证: %d 个用例全部一致", env.n)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, x := range subs {
		if !contains(s, x) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
