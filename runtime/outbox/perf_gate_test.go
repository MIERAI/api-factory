package outbox_test

// 运行时的性能闸门(工厂的验证体系: 正确性之外的"不会把扩容做坏")。RUNTIME_PERF=1 时运行(CI: runtime.yml 的 perf job,
// 单独一个 job、不加 -race、只跑这个包),否则跳过。
//
// 起因: v0.5.1 在写事务里 NOTIFY → 提交时持有整个实例共用的一把锁(pg_database 上的 AccessExclusiveLock,
// database = 0)直到 WAL 落盘 → 所有写 outbox 的提交在每个 Postgres 实例上排成一队,storefront v2 扩容 1.0×。
// 功能测试全过,没有任何闸门看得见。
//
// 判定全是比值(同一台机器、同一个 Postgres、同一次运行里相比),不写死绝对数字,GitHub 的 runner 上也稳定:
//
//	① 扩展性   N 路并发写 outbox 的提交吞吐 ≥ k × 1 路(组提交: 并发的提交共用一次 WAL 落盘)
//	② 开销     N 路下经过 Writer(写 outbox + 提交后唤醒)≥ r × 只插入同样的行(没有唤醒)
//	③ 锁       N 路运行期间 pg_locks 采样: 等待全实例锁(database = 0 的共享对象)的样本占比 ≤ w
//	④ 中继延迟 提交 → published_at 的 p90 ≤ Poll / 10(唤醒有效: 不唤醒的话平均要等半个轮询间隔)
//
// 证明它能抓到回归: 把 v0.5.2 的修复(提交后唤醒)换回事务里 NOTIFY,①③ 失败(PR 说明里有数字)。

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MIERAI/api-factory/runtime/internal/itest"
	"github.com/MIERAI/api-factory/runtime/message"
	"github.com/MIERAI/api-factory/runtime/outbox"
	"github.com/MIERAI/api-factory/runtime/txn"
)

// 闸门的参数(环境变量可以覆盖,便于在别的机器上校准;CI 用默认值)
var (
	perfWriters   = envInt("RUNTIME_PERF_WRITERS", 16)
	perfDur       = time.Duration(envFloat("RUNTIME_PERF_SECONDS", 2) * float64(time.Second))
	perfRounds    = envInt("RUNTIME_PERF_ROUNDS", 3)
	minScaling    = envFloat("RUNTIME_PERF_MIN_SCALING", 2.0)     // ① k
	minVsBaseline = envFloat("RUNTIME_PERF_MIN_VS_BASELINE", 0.6) // ② r
	maxLockWaits  = envFloat("RUNTIME_PERF_MAX_LOCK_WAITS", 0.02) // ③ w
)

func perfOn(t *testing.T) {
	if os.Getenv("RUNTIME_PERF") != "1" {
		t.Skip("RUNTIME_PERF=1 to run the runtime's performance gate")
	}
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil && v > 0 {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil && v > 0 {
		return v
	}
	return def
}

func perfPool(t *testing.T, dbURL string, conns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec // 和 server 一样
	cfg.MaxConns = conns
	cfg.MinConns = conns
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// commitRate: writers 个 goroutine 在 perfDur 里不停地提交一个写 outbox 的事务(一行),返回每秒提交数。
// baseline = 只插入同样的一行,不经过 Writer(没有提交后的唤醒)。
func commitRate(t *testing.T, e *txn.Executor, w *outbox.Writer, writers int, baseline bool) float64 {
	t.Helper()
	var n atomic.Int64
	var failed atomic.Value
	deadline := time.Now().Add(perfDur)
	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				err := e.InTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
					if baseline {
						_, err := tx.Exec(ctx, `INSERT INTO factory_outbox (id, kind, type, version, subject, envelope, occurred_at)
VALUES (gen_random_uuid(), 1, 'perf', 1, '', '{}', now())`)
						return err
					}
					_, err := w.Write(ctx, tx, outbox.Message{Kind: message.Event, Type: "perf", Version: 1})
					return err
				})
				if err != nil {
					failed.Store(err)
					return
				}
				n.Add(1)
			}
		}()
	}
	wg.Wait()
	if err, ok := failed.Load().(error); ok {
		t.Fatal(err)
	}
	return float64(n.Load()) / perfDur.Seconds()
}

// lockSampler: 另一条连接每 2ms 看一次 pg_locks: 有没有后端在等全实例的锁(database = 0: 共享对象,例如 NOTIFY 提交时
// 拿的 pg_database 上的锁)。返回 (有等待的样本数, 样本数)。
func lockSampler(t *testing.T, dbURL string) (stop func() (waits, samples int, seen map[string]int)) {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var waits, samples int
	seen := map[string]int{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			rows, err := c.Query(ctx, `SELECT locktype || ' ' || coalesce(classid::regclass::text, '') || ' ' || mode
FROM pg_locks WHERE NOT granted AND database IS NOT DISTINCT FROM 0`)
			if err != nil {
				return
			}
			any := false
			for rows.Next() {
				var s string
				if rows.Scan(&s) == nil {
					seen[s]++
					any = true
				}
			}
			rows.Close()
			samples++
			if any {
				waits++
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return func() (int, int, map[string]int) {
		cancel()
		<-done
		_ = c.Close(context.Background())
		return waits, samples, seen
	}
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// ①②③: 写 outbox 的提交在并发下不排队。
func TestPerfOutboxCommitScaling(t *testing.T) {
	perfOn(t)
	dbURL := itest.NewDB(t)
	pool := perfPool(t, dbURL, int32(perfWriters)+4)
	e := txn.New(pool)
	w := outbox.NewWriter("rt-perf")
	commitRate(t, e, w, perfWriters, false) // 预热(连接、缓存、WAL 段)

	// 交替测几轮取中位数: 机器上别的负载的波动对每种模式的影响差不多
	var one, many, base []float64
	var waits, samples int
	waiting := map[string]int{} // 等待的锁(类型 对象 模式)→ 出现的次数
	for i := 0; i < perfRounds; i++ {
		one = append(one, commitRate(t, e, w, 1, false))
		stop := lockSampler(t, dbURL)
		many = append(many, commitRate(t, e, w, perfWriters, false))
		wt, sm, seen := stop()
		waits, samples = waits+wt, samples+sm
		for k, v := range seen {
			waiting[k] += v
		}
		base = append(base, commitRate(t, e, w, perfWriters, true))
	}
	detail := fmt.Sprint(waiting)
	r1, rN, rB := median(one), median(many), median(base)
	scaling, vsBase, waitShare := rN/r1, rN/rB, float64(waits)/float64(max(samples, 1))
	report := fmt.Sprintf("commits/s: 1 writer %.0f, %d writers %.0f (×%.2f), %d writers without the wake-up %.0f (writer/baseline %.2f); "+
		"pg_locks: %d of %d samples waiting on a cluster-wide lock (%.1f%%) %s",
		r1, perfWriters, rN, scaling, perfWriters, rB, vsBase, waits, samples, 100*waitShare, detail)
	t.Log(report)
	if summary := os.Getenv("RUNTIME_PERF_SUMMARY"); summary != "" { // CI: 写进 job summary
		if f, err := os.OpenFile(summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "| outbox commit throughput | 1 writer %.0f/s, %d writers %.0f/s | scaling ×%.2f (≥ %.1f) | writer/baseline %.2f (≥ %.1f) | lock-wait samples %.1f%% (≤ %.0f%%) |\n",
				r1, perfWriters, rN, scaling, minScaling, vsBase, minVsBaseline, 100*waitShare, 100*maxLockWaits)
			_ = f.Close()
		}
	}
	if scaling < minScaling {
		t.Errorf("① outbox commits do not scale: %d writers ×%.2f of 1 writer (gate ≥ ×%.1f) — commits serialize (a NOTIFY or another cluster-wide lock in the write transaction?)", perfWriters, scaling, minScaling)
	}
	if vsBase < minVsBaseline {
		t.Errorf("② the outbox writer costs too much: %.2f of the plain insert's throughput at %d writers (gate ≥ %.1f)", vsBase, perfWriters, minVsBaseline)
	}
	if waitShare > maxLockWaits {
		t.Errorf("③ commits wait on a cluster-wide lock in %.1f%% of the pg_locks samples (gate ≤ %.0f%%): %s", 100*waitShare, 100*maxLockWaits, detail)
	}
}

// ④: 中继的唤醒有效 —— 提交到发出的延迟远小于轮询间隔。
func TestPerfRelayLatency(t *testing.T) {
	perfOn(t)
	sfx := itest.Suffix()
	evt := "evt.perf." + sfx
	setupQueues(t, evt)
	pool := itest.Pool(t, itest.NewDB(t))
	poll := 2 * time.Second
	stop := runRelay(t, &outbox.Relay{DB: pool, Broker: itest.Broker(t), Log: quiet, Poll: poll})
	defer stop()
	time.Sleep(500 * time.Millisecond) // 中继已经在 LISTEN
	var lat []time.Duration
	for i := 0; i < 60; i++ {
		envs := writeMessages(t, pool, outbox.Message{Kind: message.Event, Type: evt, Version: 1})
		committed := time.Now()
		var published *time.Time
		for start := time.Now(); time.Since(start) < 3*poll; time.Sleep(time.Millisecond) {
			if err := pool.QueryRow(context.Background(), `SELECT published_at FROM factory_outbox WHERE id = $1::uuid`, envs[0].ID).Scan(&published); err != nil {
				t.Fatal(err)
			}
			if published != nil {
				break
			}
		}
		if published == nil {
			t.Fatalf("message %d not published within %v", i, 3*poll)
		}
		lat = append(lat, time.Since(committed))
		time.Sleep(time.Duration(i%7) * 10 * time.Millisecond) // 不和轮询的节拍对齐
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p50, p90 := lat[len(lat)/2], lat[len(lat)*9/10]
	t.Logf("relay commit → published (observed by polling every 1ms) over %d messages: p50 %v p90 %v max %v; poll %v", len(lat), p50, p90, lat[len(lat)-1], poll)
	if summary := os.Getenv("RUNTIME_PERF_SUMMARY"); summary != "" {
		if f, err := os.OpenFile(summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "| relay latency | p50 %v, p90 %v | p90/poll %.3f (≤ 0.1) | | |\n", p50.Round(time.Millisecond), p90.Round(time.Millisecond), float64(p90)/float64(poll))
			_ = f.Close()
		}
	}
	if p90 > poll/10 {
		t.Errorf("④ relay latency p90 %v > poll/10 (%v): the after-commit wake-up does not reach the relay", p90, poll/10)
	}
}
