package core

// local_date(今天)的元件认证: Postgres 上 SQL 后端的结果 vs 参考求值器,逐值比较。
// 值域: 时间 = 东京 / 纽约 / UTC 的日界线前后(−1 秒 / 正好 / +1 秒)、月末、闰年、夏令时的切换日;
// now = 同样的几个时刻(日界线前后: UTC 的日期和东京的日期不同)。会话时区故意不是 UTC。
// 用法: 值(project)/ 今天 和日期的列比较(= / < / <= / >=,今天 − n 天、月初(今天))/ 和整数编码的日期的列比较
// (ymd(列) 的肯定位置改写成整数的比较)/ 子表的条件(nest: 子行在今天有效)。

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestCertificationLocalDate(t *testing.T) {
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
	schema := fmt.Sprintf("core_cert_ld_%d", os.Getpid())
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema,
		"SET search_path = " + schema, "SET TimeZone = 'America/Los_Angeles'",
		`CREATE TABLE ld (k int8 PRIMARY KEY, dt date, ts timestamptz, n int4)`,
		`CREATE TABLE lv (k int8, seq int4, vf date, vt date, PRIMARY KEY (k, seq))`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	env := &certEnv{t: t, ctx: ctx, conn: conn, table: "ld"}
	cols := []NamedType{{"k", Type{Kind: Int64}}, {"dt", Type{Kind: Date, Nullable: true}}, {"ts", Type{Kind: Timestamp, Nullable: true}}, {"n", Type{Kind: Int32, Nullable: true}}}
	var instants []time.Time
	for _, d := range []string{"2026-01-31", "2026-02-28", "2024-02-29", "2026-03-08", "2026-11-01", "2026-12-31"} {
		base := day(d)
		for _, off := range []time.Duration{0, 15 * time.Hour, 5 * time.Hour, 4 * time.Hour} { // UTC / 东京 / 纽约(标准 / 夏令)的日界线
			for _, s := range []time.Duration{-time.Second, 0, time.Second} {
				instants = append(instants, base.Add(off+s))
			}
		}
	}
	k := int64(0)
	ins := func(dt, ts, n any) {
		k++
		if _, err := conn.Exec(ctx, "INSERT INTO ld VALUES ($1, $2::date, $3::timestamptz, $4::int4)", k, sqlArg(dt), sqlArg(ts), n); err != nil {
			t.Fatal(err)
		}
		env.rows = append(env.rows, Row{"k": k, "dt": dt, "ts": ts, "n": n})
	}
	ins(nil, nil, nil)
	for i, x := range instants {
		d := time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, time.UTC)
		ins(d.AddDate(0, 0, i%3-1), x, YMDInt(d.AddDate(0, 0, (i+1)%3-1)))
	}
	ins(nil, nil, int64(20260230)) // 不合法的整数: 永远不命中
	ins(nil, nil, int64(20260229))
	scan := Scan("ld", cols...)
	byK := SortKey{Col: "k"}
	zonesUnderTest := []string{"Asia/Tokyo", "UTC", "America/New_York"}
	// 1. 值: local_date(ts)
	for _, z := range zonesUnderTest {
		env.check("local_date 值 "+z, &Pipeline{Steps: []Step{scan, Project(NamedExpr{"k", Col("k")}, NamedExpr{"v", LocalDate(Col("ts"), z)}), Sort(byK)}}, nil)
	}
	// 2. 今天 和日期的列 / 整数编码的日期的列比较(now = 日界线前后的时刻)
	one := Const(int64(1), Type{Kind: Int32})
	for _, now := range []time.Time{ts("2026-02-28T14:59:59Z"), ts("2026-02-28T15:00:00Z"), ts("2024-02-29T15:00:01Z"), ts("2026-12-31T23:59:59Z"), ts("2026-03-08T05:00:00Z")} {
		for _, z := range zonesUnderTest {
			for op := range cmpOps {
				for _, v := range []*Expr{Today(z), Add(Today(z), one, Day, true, z), Trunc(Today(z), Month, z)} {
					fv, _ := Format(v)
					p := &Pipeline{Steps: []Step{scan, Filter(Cmp(op, Col("dt"), v)), Sort(byK)}}
					env.check(fmt.Sprintf("dt %s %s [%s] now=%s", op, fv, z, now.Format(time.RFC3339)), p, Params{NowParam: now})
					p = &Pipeline{Steps: []Step{scan, Filter(Cmp(op, YMD(Col("n")), v)), Sort(byK)}}
					env.check(fmt.Sprintf("ymd(n) %s %s [%s] now=%s", op, fv, z, now.Format(time.RFC3339)), p, Params{NowParam: now})
				}
			}
		}
	}
	// 3. 子表的条件: 子行在今天有效(vf ≤ 今天,vt 为空或 今天 ≤ vt)
	var child []Row
	for i := int64(1); i <= k; i += 2 {
		for seq, w := range [][2]string{{"2026-02-01", "2026-02-28"}, {"2026-03-01", ""}, {"2024-02-29", "2024-02-29"}, {"2026-12-31", ""}} {
			var vt any
			if w[1] != "" {
				vt = day(w[1])
			}
			if _, err := conn.Exec(ctx, "INSERT INTO lv VALUES ($1, $2, $3::date, $4::date)", i, seq, w[0], sqlArg(vt)); err != nil {
				t.Fatal(err)
			}
			child = append(child, Row{"k": i, "seq": int64(seq), "vf": day(w[0]), "vt": vt})
		}
	}
	lvCols := []NamedType{{"k", Type{Kind: Int64}}, {"seq", Type{Kind: Int32}}, {"vf", Type{Kind: Date}}, {"vt", Type{Kind: Date, Nullable: true}}}
	valid := And(Cmp("<=", Col("vf"), Today("Asia/Tokyo")), Or(IsNull(Col("vt")), Cmp(">=", Col("vt"), Today("Asia/Tokyo"))))
	nestEnv := &certEnv{t: t, ctx: ctx, conn: conn, table: "ld", rows: env.rows, extra: map[string][]Row{"lv": child}}
	for _, now := range []time.Time{ts("2026-02-28T14:59:59Z"), ts("2026-02-28T15:00:00Z"), ts("2024-02-29T03:00:00Z"), ts("2026-12-30T15:00:00Z")} {
		p := &Pipeline{Steps: []Step{scan, Nest("lv", lvCols, [][]string{{"k", "seq"}}, []JoinKey{{Col: "k", Key: "k"}}, valid, []SortKey{{Col: "seq"}}, 9, "items",
			[]TakeCol{{Name: "seq", Col: "seq"}, {Name: "vf", Col: "vf"}}), Project(NamedExpr{"k", Col("k")}, NamedExpr{"items", Col("items")}), Sort(byK)}}
		nestEnv.check("nest 今天有效 now="+now.Format(time.RFC3339), p, Params{NowParam: now})
	}
	// 4. SQL 的写法: 领域的时区(不受会话时区影响);整数编码的日期的列在肯定位置按整数比较
	s, err := ToSQL(&Pipeline{Steps: []Step{scan, Filter(Cmp("<=", YMD(Col("n")), Today("Asia/Tokyo")))}}, Options{})
	if err != nil || !containsAll(s.Text, "(($1::timestamptz AT TIME ZONE 'Asia/Tokyo')::date)", "(t1.n <= (to_char(") {
		t.Errorf("SQL: %v\n%s", err, s.Text)
	}
	if p := (&Pipeline{Steps: []Step{scan, Filter(Cmp("=", Col("dt"), Today("")))}}); !p.UsesNow() {
		t.Errorf("今天 用到 now(适配器要接时钟)")
	}
	if !t.Failed() {
		t.Logf("local_date 的元件认证: %d 个用例全部一致", env.n+nestEnv.n)
	}
}
