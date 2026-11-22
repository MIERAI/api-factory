package core

// 时间运算的元件认证(now / add / trunc): Postgres 上 SQL 后端的结果 vs 参考求值器(timeops.go,不调用数据库),逐值比较。
// 值域: 2023~2025 年每个月的 1 / 28 / 29 / 30 / 31 日(每个月末、闰年 2 月)+ 世纪的闰年规则(1900 / 2000 / 2100),
// 时间 = 这些日子的 UTC 0 点、东京的日界线前后(14:59:59 / 15:00:00 / 15:00:01 UTC)、纽约的日界线、带小数的秒;
// 个数 = 负数、0、跨年的 ±11~13 / ±25。数据库会话的时区故意不是 UTC(时区必须来自领域,不能来自会话)。

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

var timeCols = []NamedType{{"k", Type{Kind: Int64}}, {"dt", Type{Kind: Date, Nullable: true}}, {"ts", Type{Kind: Timestamp, Nullable: true}}}

func TestCertificationTime(t *testing.T) {
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
	schema := fmt.Sprintf("core_cert_time_%d", os.Getpid())
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema,
		// 会话时区: 既不是 UTC 也不是东京。add / trunc 如果用了会话时区,结果会不同(::text 的输出另外按 UTC 读,见下)
		"SET search_path = " + schema, "SET TimeZone = 'America/Los_Angeles'",
		`CREATE TABLE tm (k int8 PRIMARY KEY, dt date, ts timestamptz)`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	env := &certEnv{t: t, ctx: ctx, conn: conn, table: "tm"}
	var days []time.Time
	for y := 2023; y <= 2025; y++ {
		for m := time.January; m <= time.December; m++ {
			for _, d := range []int{1, 28, 29, 30, 31} {
				if d <= daysInRef(y, m) {
					days = append(days, time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
				}
			}
		}
	}
	for _, s := range []string{"2025-03-08", "2025-11-01", "2024-03-09", "1900-02-28", "1900-03-31", "2000-01-31", "2000-02-29", "2100-02-28", "2100-01-31", "2024-02-29"} {
		days = append(days, day(s))
	}
	k := int64(0)
	ins := func(dt, ts any) {
		k++
		if _, err := conn.Exec(ctx, "INSERT INTO tm VALUES ($1, $2::date, $3::timestamptz)", k, sqlArg(dt), sqlArg(ts)); err != nil {
			t.Fatal(err)
		}
		env.rows = append(env.rows, Row{"k": k, "dt": dt, "ts": ts})
	}
	ins(nil, nil)
	for _, d := range days {
		ins(d, nil)
		for _, off := range []time.Duration{0, 9*time.Hour + 30*time.Minute + 500*time.Millisecond, 14*time.Hour + 59*time.Minute + 59*time.Second,
			15 * time.Hour, 15*time.Hour + time.Second, 23*time.Hour + 59*time.Minute + 59*time.Second + 999999*time.Microsecond, 4 * time.Hour, 5 * time.Hour,
			7*time.Hour + 30*time.Minute /* 纽约 02:30(标准时间): 加天数落在夏令时开始的那一天 → 不存在的时刻 */} {
			ins(nil, d.Add(off))
		}
	}
	run := func(name string, p *Pipeline, params Params) {
		t.Helper()
		env.check(name, p, params)
	}
	scan := Scan("tm", timeCols...)
	byK := SortKey{Col: "k"}
	ns := []int64{-25, -13, -12, -11, -2, -1, 0, 1, 2, 11, 12, 13, 25}
	zonesUnderTest := []string{"Asia/Tokyo", "UTC", "America/New_York"}
	proj := func(e *Expr) []Step {
		return []Step{scan, Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", e}), Sort(byK)}
	}
	n32 := ParamDecl{Name: "n", Type: Type{Kind: Int32}}
	cases := 0
	for _, sessionTZ := range []string{"UTC", "America/Los_Angeles"} {
		if _, err := conn.Exec(ctx, "SET TimeZone = '"+sessionTZ+"'"); err != nil {
			t.Fatal(err)
		}
		// 会话时区不是 UTC 时,时间的输出文本带别的偏移(FromPGText 按偏移读,是同一个时刻)
		// 1. 日期 + n 天 / 月(n = 参数,正负;neg = 减)
		for _, unit := range []string{Day, Month} {
			for _, n := range ns {
				for _, neg := range []bool{false, true} {
					p := &Pipeline{Params: []ParamDecl{n32}, Steps: proj(Add(Col("dt"), Param("n"), unit, neg, "Asia/Tokyo"))}
					run(fmt.Sprintf("[%s] 日期 add %d %s neg=%v", sessionTZ, n, unit, neg), p, Params{"n": n})
					cases++
				}
			}
		}
		// 2. 时间 + n 分 / 时 / 天 / 月(每个时区)
		for _, z := range zonesUnderTest {
			for _, unit := range []string{Minute, Hour, Day, Month} {
				for _, n := range ns {
					p := &Pipeline{Params: []ParamDecl{n32}, Steps: proj(Add(Col("ts"), Param("n"), unit, false, z))}
					run(fmt.Sprintf("[%s] 时间 add %d %s %s", sessionTZ, n, unit, z), p, Params{"n": n})
					cases++
				}
			}
			// 3. trunc(天 / 月),截了再加(上个月的期间)
			for _, unit := range []string{Day, Month} {
				run(fmt.Sprintf("[%s] 时间 trunc %s %s", sessionTZ, unit, z), &Pipeline{Steps: proj(Trunc(Col("ts"), unit, z))}, nil)
				run(fmt.Sprintf("[%s] 时间 trunc(add -1 month) %s %s", sessionTZ, unit, z),
					&Pipeline{Steps: proj(Trunc(Add(Col("ts"), Const(int64(-1), Type{Kind: Int32}), Month, false, z), unit, z))}, nil)
				cases += 2
			}
		}
		for _, unit := range []string{Day, Month} {
			run(fmt.Sprintf("[%s] 日期 trunc %s", sessionTZ, unit), &Pipeline{Steps: proj(Trunc(Col("dt"), unit, "Asia/Tokyo"))}, nil)
			cases++
		}
		// 上个月的期间(设计稿 §2.5): [trunc(add(d, -1, month), month), add(trunc(d, month), -1, day)]
		run(fmt.Sprintf("[%s] 上个月的开始", sessionTZ), &Pipeline{Steps: proj(Trunc(Add(Col("dt"), Const(int64(1), Type{Kind: Int32}), Month, true, ""), Month, ""))}, nil)
		run(fmt.Sprintf("[%s] 上个月的结束", sessionTZ), &Pipeline{Steps: proj(Add(Trunc(Col("dt"), Month, ""), Const(int64(-1), Type{Kind: Int32}), Day, false, ""))}, nil)
		cases += 2
		// 4. 个数: int8 的参数(cast int4)、NULL(不传)→ NULL、常量
		n64 := ParamDecl{Name: "n", Type: Type{Kind: Int64}}
		p64 := &Pipeline{Params: []ParamDecl{n64}, Steps: proj(Add(Col("ts"), Param("n"), Month, true, "Asia/Tokyo"))}
		for _, ps := range []Params{{"n": int64(1)}, {"n": int64(-14)}, {}} {
			run(fmt.Sprintf("[%s] int8 的个数 %v", sessionTZ, ps), p64, ps)
			cases++
		}
		pd := &Pipeline{Params: []ParamDecl{n64}, Steps: proj(Add(Col("dt"), Param("n"), Day, false, ""))}
		run(fmt.Sprintf("[%s] 日期 + NULL 天", sessionTZ), pd, Params{})
		cases++
	}
	// 5. now() + 筛选: 早于 now − n 分钟(边界上的行: 正好等于 → 不是"早于")
	if _, err := conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		t.Fatal(err)
	}
	nowP := &Pipeline{Params: []ParamDecl{{Name: "n", Type: Type{Kind: Int32}, Required: true}}}
	for _, now := range []time.Time{ts("2024-02-29T15:00:00Z"), ts("2025-01-01T00:00:00Z"), ts("2023-03-31T15:00:01Z")} {
		for _, n := range []int64{1, 60, 1440} {
			for _, op := range []string{"<", "<="} {
				nowP.Steps = []Step{scan, Filter(Cmp(op, Col("ts"), Add(Now(), Param("n"), Minute, true, "Asia/Tokyo"))), Sort(byK)}
				run(fmt.Sprintf("now %s − %d 分钟 (%s)", op, n, now.Format(time.RFC3339)), nowP, Params{"n": n, NowParam: now})
				cases++
			}
		}
		// 今天(领域的时区)开始以后
		nowP.Steps = []Step{scan, Filter(Cmp(">=", Col("ts"), Trunc(Now(), Day, "Asia/Tokyo"))), Sort(byK)}
		run("今天以后 "+now.Format(time.RFC3339), nowP, Params{"n": int64(1), NowParam: now})
		cases++
	}
	// 6. 超出 int4 的个数: 两边都报错
	pbig := &Pipeline{Params: []ParamDecl{{Name: "n", Type: Type{Kind: Int64}}}, Steps: proj(Add(Col("ts"), Param("n"), Day, false, "UTC"))}
	if _, werr := Eval(pbig, map[string][]Row{"tm": env.rows}, Params{"n": int64(1) << 40}); werr == nil {
		t.Errorf("超出 int4: 参考求值器应该报错")
	}
	if _, _, gerr := env.run(pbig, Params{"n": int64(1) << 40}); gerr == nil {
		t.Errorf("超出 int4: SQL 应该报错")
	}
	if !t.Failed() {
		t.Logf("时间运算的元件认证: %d 个用例 × %d 行全部一致", env.n, len(env.rows))
	}
}

// daysInRef: 认证数据用的月的天数(用 time 包的规范化算,和 timeops.go 的 daysIn 互相独立)。
func daysInRef(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// 参考实现自己的单元测试(不需要数据库): 月末、闰年、负数、时区。
func TestTimeOpsReference(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int64
		unit string
		want string
	}{
		{"2026-01-31", 1, Month, "2026-02-28"}, {"2024-01-31", 1, Month, "2024-02-29"}, {"2024-02-29", 12, Month, "2025-02-28"},
		{"2026-03-31", -1, Month, "2026-02-28"}, {"2026-01-15", -13, Month, "2024-12-15"}, {"2000-01-31", 1, Month, "2000-02-29"},
		{"1900-01-31", 1, Month, "1900-02-28"}, {"2026-12-31", 1, Day, "2027-01-01"}, {"2026-03-01", -1, Day, "2026-02-28"},
	} {
		got, err := addTime(day(c.in), c.n, c.unit, Date, "")
		if err != nil || got.Format("2006-01-02") != c.want {
			t.Errorf("%s + %d %s = %v %v,期待 %s", c.in, c.n, c.unit, got, err, c.want)
		}
	}
	// 东京 2026-01-31 05:00(= 01-30 20:00Z)+ 1 个月 = 东京 02-28 05:00(按东京的日历,不是 UTC 的 01-30 → 02-28)
	got, _ := addTime(ts("2026-01-30T20:00:00Z"), 1, Month, Timestamp, "Asia/Tokyo")
	if got.Format(time.RFC3339) != "2026-02-27T20:00:00Z" {
		t.Errorf("东京的月加法: %v", got)
	}
	got, _ = truncTime(ts("2026-01-31T15:00:00Z"), Month, Timestamp, "Asia/Tokyo") // 东京 2/1 0:00
	if got.Format(time.RFC3339) != "2026-01-31T15:00:00Z" {
		t.Errorf("东京的月初: %v", got)
	}
	got, _ = truncTime(ts("2026-01-31T14:59:59Z"), Day, Timestamp, "Asia/Tokyo") // 东京 1/31 23:59:59
	if got.Format(time.RFC3339) != "2026-01-30T15:00:00Z" {
		t.Errorf("东京的日初: %v", got)
	}
}
