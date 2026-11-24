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
// 证明它能抓到回归(把 v0.5.2 的修复换回事务里 NOTIFY,api-factory #52):
//
//	                 本地 docker(fsync 贵)          GitHub runner(fsync 便宜)
//	修复后           ① 7.5×  ② 0.97  ③ 0%          ① 3.4–6.8×  ② 0.69–0.74  ③ 0%
//	事务里 NOTIFY    ① 1.5×✗ ② 0.16✗ ③ 100%✗       ① 3.1×      ② 0.47✗      ③ 99.9%✗
//
// ③ 是决定性的(锁的等待和磁盘快慢无关: 有就是有);① 只在 fsync 贵的机器上分得开(runner 上分不开),保留它是为了
// 别的原因造成的串行;② 是开销的下限(0.5: runner 上修复后最低 0.69,不让它变成偶发失败)。
//
// 追踪(tracing 包,v0.6.1 起): ①–④ 各跑两遍 —— 关闭(默认)和打开(采样 RUNTIME_PERF_TRACE_RATIO,默认 0.1 =
// parentbased_traceidratio,批量导出到本进程里的 OTLP/HTTP 收集端,和生产一样序列化、发送)。另外:
//
//	⑤ 关闭的开销   关闭时一次写事务经过的埋点(原子读、空 span、traceparent)的耗时 ≤ 1 路提交耗时的 1%(实测 ~0.002%)
//	⑥ 打开的开销   打开时 N 路的提交吞吐 ≥ t × 关闭时(交替测几轮取中位数)

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/MIERAI/api-factory/runtime/internal/itest"
	"github.com/MIERAI/api-factory/runtime/message"
	"github.com/MIERAI/api-factory/runtime/outbox"
	"github.com/MIERAI/api-factory/runtime/tracing"
	"github.com/MIERAI/api-factory/runtime/txn"
)

// 闸门的参数(环境变量可以覆盖,便于在别的机器上校准;CI 用默认值)
var (
	perfWriters   = envInt("RUNTIME_PERF_WRITERS", 16)
	perfDur       = time.Duration(envFloat("RUNTIME_PERF_SECONDS", 2) * float64(time.Second))
	perfRounds    = envInt("RUNTIME_PERF_ROUNDS", 3)
	minScaling    = envFloat("RUNTIME_PERF_MIN_SCALING", 2.0)     // ① k
	minVsBaseline = envFloat("RUNTIME_PERF_MIN_VS_BASELINE", 0.5) // ② r
	maxLockWaits  = envFloat("RUNTIME_PERF_MAX_LOCK_WAITS", 0.02) // ③ w
	traceRatio    = envFloat("RUNTIME_PERF_TRACE_RATIO", 0.1)     // 打开时的采样比例
	maxOffShare   = envFloat("RUNTIME_PERF_MAX_OFF_SHARE", 0.01)  // ⑤
	minTracingOn  = envFloat("RUNTIME_PERF_MIN_TRACING_ON", 0.7)  // ⑥ t(本机 5 轮中位数 0.87–1.04;机器忙时单轮差到 ±2×)
)

// tracingModes: 闸门的每一项在这两种状态下各跑一遍。
var tracingModes = []string{"off", "on"}

// sink: 本进程里的 OTLP/HTTP 收集端(收下就丢,只计数)。
type sink struct {
	srv      *httptest.Server
	requests atomic.Int64
	bytes    atomic.Int64
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		s.requests.Add(1)
		s.bytes.Add(n)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// tracingOn: 打开追踪(和 tracing.Setup 一样: OTLP/HTTP + 批量处理 + parentbased_traceidratio)。返回关闭的函数(先发完)。
func tracingOn(t *testing.T) (snk *sink, off func()) {
	t.Helper()
	snk = newSink(t)
	exp, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(snk.srv.URL+"/v1/traces"))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(traceRatio))))
	restore := tracing.Use(tp)
	return snk, func() {
		restore()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	}
}

// inMode: 在 mode(off / on)下跑 f;打开时连接池也装上 pgx 的 tracer(和 server 一样)。
func inMode(t *testing.T, mode string, f func(t *testing.T, traced bool)) {
	t.Run("tracing-"+mode, func(t *testing.T) {
		if mode == "off" {
			if tracing.Enabled() {
				t.Fatal("tracing is on")
			}
			f(t, false)
			return
		}
		snk, off := tracingOn(t)
		f(t, true)
		off()
		t.Logf("OTLP sink: %d requests, %d bytes", snk.requests.Load(), snk.bytes.Load())
		if snk.requests.Load() == 0 {
			t.Error("tracing on but nothing was exported")
		}
	})
}

func summaryRow(format string, args ...any) {
	if summary := os.Getenv("RUNTIME_PERF_SUMMARY"); summary != "" { // CI: 写进 job summary
		if f, err := os.OpenFile(summary, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, format, args...)
			_ = f.Close()
		}
	}
}

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

func perfPool(t *testing.T, dbURL string, conns int32, traced bool) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec // 和 server 一样
	if traced {
		cfg.ConnConfig.Tracer = tracing.PGX{}
	}
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
	for _, mode := range tracingModes {
		inMode(t, mode, func(t *testing.T, traced bool) { outboxCommitScaling(t, mode, traced) })
	}
}

func outboxCommitScaling(t *testing.T, mode string, traced bool) {
	dbURL := itest.NewDB(t)
	pool := perfPool(t, dbURL, int32(perfWriters)+4, traced)
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
	report := fmt.Sprintf("tracing %s: commits/s: 1 writer %.0f, %d writers %.0f (×%.2f), %d writers without the wake-up %.0f (writer/baseline %.2f); "+
		"pg_locks: %d of %d samples waiting on a cluster-wide lock (%.1f%%) %s",
		mode, r1, perfWriters, rN, scaling, perfWriters, rB, vsBase, waits, samples, 100*waitShare, detail)
	t.Log(report)
	summaryRow("| outbox commit throughput, tracing %s | 1 writer %.0f/s, %d writers %.0f/s | scaling ×%.2f (≥ %.1f) | writer/baseline %.2f (≥ %.1f) | lock-wait samples %.1f%% (≤ %.0f%%) |\n",
		mode, r1, perfWriters, rN, scaling, minScaling, vsBase, minVsBaseline, 100*waitShare, 100*maxLockWaits)
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
	for _, mode := range tracingModes {
		inMode(t, mode, func(t *testing.T, traced bool) { relayLatency(t, mode, traced) })
	}
}

func relayLatency(t *testing.T, mode string, traced bool) {
	sfx := itest.Suffix()
	evt := "evt.perf." + sfx
	setupQueues(t, evt)
	pool := perfPool(t, itest.NewDB(t), 8, traced)
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
	t.Logf("tracing %s: relay commit → published (observed by polling every 1ms) over %d messages: p50 %v p90 %v max %v; poll %v", mode, len(lat), p50, p90, lat[len(lat)-1], poll)
	summaryRow("| relay latency, tracing %s | p50 %v, p90 %v | p90/poll %.3f (≤ 0.1) | | |\n", mode, p50.Round(time.Millisecond), p90.Round(time.Millisecond), float64(p90)/float64(poll))
	if p90 > poll/10 {
		t.Errorf("④ relay latency p90 %v > poll/10 (%v): the after-commit wake-up does not reach the relay", p90, poll/10)
	}
}

// ⑤⑥: 追踪的开销。⑤ 关闭时: 一次写事务经过的埋点(InTx / outbox.Write / 中继 / 消费者 各一个 Enabled 判断,
// 一个空 span,message.Traceparent)耗时 ≤ 1 路提交耗时的 maxOffShare。⑥ 打开时: N 路提交吞吐 ≥ minTracingOn × 关闭时。
func TestPerfTracingOverhead(t *testing.T) {
	perfOn(t)
	if tracing.Enabled() {
		t.Fatal("tracing is on")
	}
	ctx := message.WithTraceparent(context.Background(), "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	hooks := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for j := 0; j < 4; j++ {
				_ = tracing.Enabled()
			}
			_, span := tracing.Start(ctx, "x", trace.SpanKindInternal)
			span.End()
			_ = message.Traceparent(ctx)
		}
	})
	perHook := time.Duration(hooks.NsPerOp())

	dbURL := itest.NewDB(t)
	pool := perfPool(t, dbURL, int32(perfWriters)+4, false)
	tpool := perfPool(t, dbURL, int32(perfWriters)+4, true)
	w := outbox.NewWriter("rt-perf")
	e, te := txn.New(pool), txn.New(tpool)
	commitRate(t, e, w, perfWriters, false) // 预热
	var one, off, on []float64
	var exported int64
	measureOn := func() {
		snk, stop := tracingOn(t)
		on = append(on, commitRate(t, te, w, perfWriters, false))
		stop()
		exported += snk.bytes.Load()
	}
	for i := 0; i < perfRounds+2; i++ { // 多两轮、交替先后: 两种状态受机器负载波动的影响一样
		one = append(one, commitRate(t, e, w, 1, false))
		if i%2 == 1 {
			measureOn()
		}
		off = append(off, commitRate(t, e, w, perfWriters, false))
		if i%2 == 0 {
			measureOn()
		}
	}
	r1, rOff, rOn := median(one), median(off), median(on)
	commit := time.Duration(float64(time.Second) / r1)
	offShare := float64(perHook) / float64(commit)
	onRatio := rOn / rOff
	t.Logf("tracing off: hooks %v per write transaction = %.4f%% of a 1-writer commit (%v); tracing on (ratio %.2f, OTLP %d bytes): %d writers %.0f/s vs off %.0f/s = %.2f",
		perHook, 100*offShare, commit.Round(time.Microsecond), traceRatio, exported, perfWriters, rOn, rOff, onRatio)
	summaryRow("| tracing overhead | off: hooks %v per write tx (1-writer commit %v); on (ratio %.2f): %d writers %.0f/s vs %.0f/s | off share %.4f%% (≤ %.0f%%) | on/off %.2f (≥ %.2f) | |\n",
		perHook, commit.Round(time.Microsecond), traceRatio, perfWriters, rOn, rOff, 100*offShare, 100*maxOffShare, onRatio, minTracingOn)
	if exported == 0 {
		t.Error("tracing on but nothing was exported")
	}
	if offShare > maxOffShare {
		t.Errorf("⑤ tracing off costs %.3f%% of a commit (gate ≤ %.0f%%): the off path must stay an atomic load", 100*offShare, 100*maxOffShare)
	}
	if onRatio < minTracingOn {
		t.Errorf("⑥ tracing on (ratio %.2f) leaves %.2f of the commit throughput (gate ≥ %.2f)", traceRatio, onRatio, minTracingOn)
	}
}
