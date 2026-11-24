package core

// 元件认证: 当前状态表(state.go,docs/design/current-state.md)。
//
// 状态表由生成的触发器维护,读的 SQL 从状态表读(没有取最新);参考求值器在原始表上取最新。两边必须一致:
//   - 状态表本身 = 原始表按键取最新的一行(含墓碑): 逐行比较 id
//   - 读: 状态表上的非键筛选 / 嵌套 / 关联(StateIndexCols 的形状) = 参考求值器
// 写的方式: 一条语句多行(同一个键多个版本)、登记顺序乱序 + 并列(按 id 决胜)、删除、逻辑删除、复活、可以为空的键、
// 键为 NULL(丢掉)、键的另一种合法写法(" +7 " = 7)、回滚、UPDATE / DELETE / TRUNCATE 原始表(数据修复)、
// 先有数据再建(回填)、DDL 再执行一次(幂等)、并发(16 个连接写同几个键;READ COMMITTED 和 REPEATABLE READ)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCertificationState(t *testing.T) {
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
	schema := fmt.Sprintf("core_cert_state_%d", os.Getpid())
	exec := func(c interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, q string, args ...any) {
		t.Helper()
		if _, err := c.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	for _, q := range []string{
		"DROP SCHEMA IF EXISTS " + schema + " CASCADE", "CREATE SCHEMA " + schema, "SET search_path = " + schema,
		`CREATE TABLE pm (k int8 PRIMARY KEY, a int4, cat text COLLATE "en-US-x-icu")`,
		`CREATE TABLE ev (id int8 PRIMARY KEY, data jsonb NOT NULL, action int4 NOT NULL, reg int8 NOT NULL)`,
		`CREATE TABLE evb (id int8 PRIMARY KEY, data jsonb NOT NULL, action int4 NOT NULL, reg int8 NOT NULL)`,
	} {
		exec(conn, q)
	}
	defer conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")

	// 逻辑表: 键 (a 整数32, b 文本 可以为空);a 为 NULL 的行丢掉
	fields := []srcField{{"a", Int32}, {"b", Text}, {"cat", Text}, {"n", Decimal}}
	source := func(table, state string) []Step {
		s := jsonSource(table, fields, []string{"a", "b"}, []string{"a"})
		for i := range s {
			if s[i].Kind == "pick_per_key" {
				s[i].State = state
			}
		}
		return s
	}
	src := source("ev", "st")
	cols := srcCols(fields, []string{"a"})
	ddl := func(steps []Step, index [][]string) string {
		t.Helper()
		s, err := StateDDL(steps, index)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// 索引的列 = 读它的流水线用到的(和编译器一样从流水线算)
	pmScan := Scan("pm", NamedType{"k", Type{Kind: Int64}}, NamedType{"a", Type{Kind: Int32, Nullable: true}}, NamedType{"cat", Type{Kind: Text, Nullable: true}})
	catParam := &Pipeline{Params: []ParamDecl{{Name: "c", Type: Type{Kind: Text}}}, Steps: append(append([]Step(nil), src...),
		Filter(Or(Absent("c"), Cmp("=", Col("cat"), Param("c")))), Sort(SortKey{Col: "a"}, SortKey{Col: "b"}))}
	nest := Nest("st", cols, [][]string{{"a", "b"}}, []JoinKey{{"cat", "cat"}}, nil, []SortKey{{"a", false}, {"b", false}}, 3, "items",
		[]TakeCol{{"a", "a"}, {"b", "b"}, {"n", "n"}})
	nest.Source = src
	nestP := &Pipeline{Steps: []Step{pmScan, nest, Sort(SortKey{Col: "k"})}}
	look := Lookup("st", cols, [][]string{{"a", "b"}}, []JoinKey{{"a", "a"}}, []TakeCol{{"x_cat", "cat"}, {"x_n", "n"}}, "keep")
	look.Source, look.Pred = src, Cmp("=", Col("b"), Const("x", Type{Kind: Text}))
	lookP := &Pipeline{Steps: []Step{pmScan, look, Sort(SortKey{Col: "k"})}}
	index := map[string][][]string{}
	for _, p := range []*Pipeline{catParam, nestP, lookP} {
		for st, g := range StateIndexCols(p) {
			index[st] = append(index[st], g...)
		}
	}
	if len(index["st"]) != 3 {
		t.Errorf("索引的列: %v(应该有 cat / cat+顺序 / a+b)", index["st"])
	}
	exec(conn, ddl(src, index["st"]))
	exec(conn, ddl(src, index["st"])) // 幂等

	env := &certEnv{t: t, ctx: ctx, conn: conn, table: "pm"}
	for i, r := range [][2]any{{1, "x"}, {2, "y"}, {3, nil}, {nil, "x"}, {4, "x"}, {7, ""}, {5, "z"}} {
		exec(conn, "INSERT INTO pm VALUES ($1, $2, $3)", int64(i+1), r[0], r[1])
		var a any
		if r[0] != nil {
			a = int64(r[0].(int))
		}
		env.rows = append(env.rows, Row{"k": int64(i + 1), "a": a, "cat": r[1]})
	}
	reload := func(table string) []Row { // 参考求值器的数据 = 原始表现在的内容
		t.Helper()
		rows, err := conn.Query(ctx, "SELECT id, data::text, action, reg FROM "+table+" ORDER BY id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []Row
		for rows.Next() {
			var id, reg int64
			var action int32
			var data string
			if err := rows.Scan(&id, &data, &action, &reg); err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(strings.NewReader(data))
			dec.UseNumber()
			var doc any
			_ = dec.Decode(&doc)
			out = append(out, Row{"id": id, "data": doc, "action": int64(action), "reg": reg})
		}
		return out
	}
	// 状态表本身 = 原始表按键取最新(墓碑也在): 比较 id 的集合
	checkState := func(name, table, state string, steps []Step) {
		t.Helper()
		env.n++
		k := statePrefix(steps)
		pick := steps[k]
		pick.State = ""
		ref, err := Eval(&Pipeline{Steps: append(append([]Step(nil), steps[:k]...), pick)}, map[string][]Row{table: reload(table)}, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var want, got []string
		for _, r := range ref.Rows {
			want = append(want, Canonical(r["_o_id"]))
		}
		rows, err := conn.Query(ctx, "SELECT id::text FROM "+state)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			got = append(got, s)
		}
		rows.Close()
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("%s: 状态表和原始表的取最新不一致\n参考求值器: %v\n状态表:     %v", name, want, got)
		}
	}
	t0 := time.Now()
	checkReads := func(name string) {
		t.Helper()
		t.Logf("%s: %v", name, time.Since(t0).Round(time.Millisecond))
		env.others = map[string][]Row{"ev": reload("ev")}
		checkState(name, "ev", "st", src)
		for _, c := range []any{"<absent>", "x", "y", ""} {
			ps := Params{}
			if c != "<absent>" {
				ps["c"] = c
			}
			env.check(fmt.Sprintf("%s: 非键筛选 cat=%v", name, c), catParam, ps)
		}
		env.check(name+": 嵌套(非键关联)", nestP, nil)
		env.check(name+": 关联(对方的列 = 常量)", lookP, nil)
	}

	rnd := rand.New(rand.NewSource(1))
	id := int64(0)
	keyA := []string{`1`, `2`, `" +2 "`, `"07"`, `7`, `4`, `null`, `3`} // " +2 " = 2、"07" = 7(同一个键的另一种合法写法)
	keyB := []string{`"x"`, `"X"`, `""`, `null`, `"y"`, ``}           // `` = 没有这个键(= NULL,可以为空的键)
	version := func() (string, int64) {
		var parts []string
		parts = append(parts, `"a": `+keyA[rnd.Intn(len(keyA))])
		if b := keyB[rnd.Intn(len(keyB))]; b != "" {
			parts = append(parts, `"b": `+b)
		}
		parts = append(parts, fmt.Sprintf(`"cat": %q`, []string{"x", "y", "", "z"}[rnd.Intn(4)]), fmt.Sprintf(`"n": "%d.%d"`, rnd.Intn(9), rnd.Intn(99)))
		if rnd.Intn(6) == 0 {
			parts = append(parts, `"del": "1"`)
		}
		action := int64(0)
		if rnd.Intn(5) == 0 {
			action = 1
		}
		return "{" + strings.Join(parts, ", ") + "}", action
	}
	insert := func(c interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, table string, n int) {
		var vals []string
		var args []any
		for i := 0; i < n; i++ {
			id++
			data, action := version()
			reg := int64(rnd.Intn(20)) // 乱序 + 并列(按 id 决胜)
			vals = append(vals, fmt.Sprintf("($%d, $%d::jsonb, $%d, $%d)", len(args)+1, len(args)+2, len(args)+3, len(args)+4))
			args = append(args, id, data, action, reg)
		}
		exec(c, "INSERT INTO "+table+" VALUES "+strings.Join(vals, ", "), args...)
	}
	// 1. 一条语句多行 / 一行一条
	for round := 0; round < 40; round++ {
		insert(conn, "ev", 1+rnd.Intn(5))
		if round%8 == 7 {
			checkReads(fmt.Sprintf("写入 %d 轮", round+1))
		}
	}
	// 2. 回滚: 状态表也不变
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insert(tx, "ev", 6)
	_ = tx.Rollback(ctx)
	checkReads("回滚")
	// 3. 数据修复: UPDATE / DELETE 原始表 → 按键重算
	exec(conn, `UPDATE ev SET reg = reg + 100 WHERE id % 7 = 0`)
	checkReads("UPDATE 登记顺序")
	exec(conn, `UPDATE ev SET data = jsonb_set(data, '{a}', '5') WHERE id % 11 = 0`)
	checkReads("UPDATE 键")
	exec(conn, `DELETE FROM ev WHERE id % 5 = 0`)
	checkReads("DELETE")
	// 4. 键的值不合法 → 写入失败(⚖️ C4: 和读一样的带校验的转换;以前这种行让读失败)
	if _, err := conn.Exec(ctx, `INSERT INTO ev VALUES (100000, '{"a": "abc"}', 0, 1)`); err == nil {
		t.Error("键的值不合法: 写入应该失败")
	}
	// 5. 回填: 先有数据(还没有触发器),再执行 DDL = 一直有触发器
	srcB := source("evb", "stb")
	for i := 0; i < 30; i++ {
		insert(conn, "evb", 1+rnd.Intn(4))
	}
	exec(conn, ddl(srcB, nil))
	checkState("回填", "evb", "stb", srcB)
	insert(conn, "evb", 5)
	exec(conn, ddl(srcB, nil)) // 再执行一次(迁移重放)
	checkState("回填之后再写 + DDL 再执行", "evb", "stb", srcB)
	exec(conn, "TRUNCATE ev")
	checkReads("TRUNCATE")

	// 6. 并发: 16 个连接同时写同几个键(登记顺序随机,有的事务回滚);READ COMMITTED 和 REPEATABLE READ(序列化错误重试)
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["deadlock_timeout"] = "20ms" // 万一死锁: 快点报(重试)
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var mu sync.Mutex
	nextID := func() int64 { mu.Lock(); defer mu.Unlock(); id++; return id }
	for _, iso := range []pgx.TxIsoLevel{pgx.ReadCommitted, pgx.RepeatableRead} {
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for w := 0; w < 16; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				r := rand.New(rand.NewSource(int64(w)))
				for i := 0; i < 25; i++ {
					for attempt := 0; ; attempt++ {
						err := func() error {
							tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
							if err != nil {
								return err
							}
							defer tx.Rollback(ctx)
							keys := []int{r.Intn(3), r.Intn(3)}[:1+r.Intn(2)]
							sort.Ints(keys) // 一个事务写两个键时按键的顺序写(否则两个事务互相等 = 死锁,数据库会让其中一个失败)
							for _, a := range keys {
								data := fmt.Sprintf(`{"a": %d, "b": "x", "cat": "c%d", "n": "%d"}`, a, w, i)
								if _, err := tx.Exec(ctx, "INSERT INTO ev VALUES ($1, $2::jsonb, $3, $4)", nextID(), data, r.Intn(4)/3, r.Int63n(1000)); err != nil {
									return err
								}
							}
							if r.Intn(5) == 0 {
								return nil // 回滚
							}
							return tx.Commit(ctx)
						}()
						var pe *pgconn.PgError
						if err != nil && errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01") && attempt < 50 {
							continue
						}
						if err != nil {
							errs <- err
						}
						break
					}
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("并发写入: %v", err)
		}
		checkReads(fmt.Sprintf("并发(%s)", iso))
	}
	if !t.Failed() {
		t.Logf("当前状态表的元件认证: %d 个用例全部一致", env.n)
	}
}
