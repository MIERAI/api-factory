package perf

// (b) + (c): a write API under concurrency, on distinct keys (every request its own rows).
//
//	3 rounds of: 1 worker, M requests; N workers, M requests each (other rows) while pg_stat_activity / pg_locks are
//	sampled for the workers' sessions. Rates: the best round of each (other load on the machine hits both).
//
// ✗ when
//   - a worker holds a lock above row level: a relation lock stronger than ROW EXCLUSIVE, an object lock (e.g. the
//     database lock of NOTIFY at commit), an advisory lock — it serializes requests that share nothing;
//   - workers wait on locks (other than relation extension) in more than 1% of the samples of active workers: requests
//     on different keys wait for each other (a shared row: a counter, a parent updated by every request …);
//   - throughput with N workers < MinScale × the throughput with 1 worker (a ratio within one run: no absolute numbers);
//   - fewer than half of the requests succeed (the check would measure errors, not the API).

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MIERAI/api-factory/runtime/txn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const perfApp = "factory-perf-write"

func concurrency(ctx context.Context, conn *pgx.Conn, ws writeSpec, tables map[string]*table, opt Options, seq map[string]int) Report {
	n, m := opt.Workers, opt.WriteRequests
	r := Report{API: ws.api.ID, Variant: fmt.Sprintf("%s: 并发 %d 路", ws.variant, n)}
	fail := func(format string, args ...any) Report {
		r.Fail = append(r.Fail, fmt.Sprintf(format, args...))
		return r
	}
	const rounds = 3 // 1 路 / N 路 alternate; the best rate of each: load from elsewhere on the machine hits both
	per := m + n*m
	envs, err := genRequests(ctx, conn, ws, tables, rounds*per, seq, opt.Pick)
	if err != nil {
		return fail("造请求: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(opt.DB)
	if err != nil {
		return fail("%v", err)
	}
	cfg.MaxConns = int32(n + 1)
	cfg.ConnConfig.RuntimeParams["application_name"] = perfApp
	cfg.ConnConfig.RuntimeParams["TimeZone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fail("%v", err)
	}
	defer pool.Close()
	ex := txn.New(pool)
	var okN, bizN atomic.Int64
	var firstErr atomic.Value
	biz := sync.Map{}
	one := func(env *reqEnv) {
		err := ex.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return runRequest(ctx, tx, ws, env, nil) })
		switch {
		case err == nil:
			okN.Add(1)
		case business(err):
			bizN.Add(1)
			reason := txn.ReasonOf(err).String()
			v, _ := biz.LoadOrStore(reason, new(atomic.Int64))
			v.(*atomic.Int64).Add(1)
		default:
			firstErr.CompareAndSwap(nil, err.Error())
		}
	}
	// warm the pool (connections are not part of the measurement)
	var warm sync.WaitGroup
	for range n {
		warm.Add(1)
		go func() { defer warm.Done(); c, err := pool.Acquire(ctx); if err == nil { time.Sleep(20 * time.Millisecond); c.Release() } }()
	}
	warm.Wait()
	stop := make(chan struct{})
	var samples, waits int
	waitBy := map[string]int{}
	strong := map[string]int{}
	var inN atomic.Bool
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !inN.Load() { // sample the N-worker phases only
				time.Sleep(time.Millisecond)
				continue
			}
			rows, err := conn.Query(ctx, `
				SELECT a.wait_event_type IS NOT DISTINCT FROM 'Lock', coalesce(a.wait_event, ''),
				       coalesce(l.locktype, ''), coalesce(l.relation::regclass::text, '')
				FROM pg_stat_activity a LEFT JOIN pg_locks l ON l.pid = a.pid AND NOT l.granted
				WHERE a.application_name = $1 AND a.state = 'active' AND a.pid <> pg_backend_pid()`, perfApp)
			if err == nil {
				for rows.Next() {
					var lock bool
					var ev, lt, rel string
					if rows.Scan(&lock, &ev, &lt, &rel) != nil {
						continue
					}
					samples++
					if lock && ev != "extend" {
						waits++
						waitBy[strings.TrimSpace(ev+" "+rel)]++
					}
				}
				rows.Close()
			}
			rows, err = conn.Query(ctx, `
				SELECT l.locktype || ' ' || l.mode || coalesce(' ' || l.relation::regclass::text, '') || coalesce(' ' || l.classid::regclass::text, '')
				FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
				WHERE a.application_name = $1 AND l.granted AND a.pid <> pg_backend_pid()
				  AND (l.locktype IN ('object', 'advisory', 'userlock')
				       OR (l.locktype = 'relation' AND l.mode NOT IN ('AccessShareLock', 'RowShareLock', 'RowExclusiveLock')))`, perfApp)
			if err == nil {
				for rows.Next() {
					var s string
					if rows.Scan(&s) == nil {
						strong[s]++
					}
				}
				rows.Close()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	rate1, rateN := 0.0, 0.0
	for k := range rounds {
		part := envs[k*per : (k+1)*per]
		t0 := time.Now()
		for _, e := range part[:m] {
			one(e)
		}
		rate1 = max(rate1, float64(m)/time.Since(t0).Seconds())
		inN.Store(true)
		t0 = time.Now()
		var wg sync.WaitGroup
		for w := range n {
			wg.Add(1)
			go func(reqs []*reqEnv) {
				defer wg.Done()
				for _, e := range reqs {
					one(e)
				}
			}(part[m+w*m : m+(w+1)*m])
		}
		wg.Wait()
		rateN = max(rateN, float64(n*m)/time.Since(t0).Seconds())
		inN.Store(false)
	}
	close(stop)
	sampler.Wait()
	total := int64(rounds * per)
	ratio := rateN / rate1
	waitPct := 0.0
	if samples > 0 {
		waitPct = 100 * float64(waits) / float64(samples)
	}
	r.Ms = 1000 * float64(n) / rateN // per request of one worker (best N-worker round)
	r.Rows = int(okN.Load())
	r.Plan = fmt.Sprintf("1 路 %.0f 请求/秒, %d 路 %.0f 请求/秒 = %.2f×; 锁等待 %.1f%%(%d 个样本)", rate1, n, rateN, ratio, waitPct, samples)
	if e, _ := firstErr.Load().(string); e != "" {
		r.Fail = append(r.Fail, "请求失败(不是业务结果): "+e)
	}
	if b := bizN.Load(); b > 0 {
		var parts []string
		biz.Range(func(k, v any) bool { parts = append(parts, fmt.Sprintf("%s %d", k, v.(*atomic.Int64).Load())); return true })
		sort.Strings(parts)
		msg := fmt.Sprintf("业务结果 %d/%d(%s)", b, total, strings.Join(parts, ", "))
		if okN.Load()*2 < total {
			r.Fail = append(r.Fail, msg+": 成功的不到一半,并发的检查没有意义(perf 造的请求不对: factory/perf/write.go)")
		} else {
			r.Warn = append(r.Warn, msg)
		}
	}
	if len(strong) > 0 {
		var ls []string
		for k := range strong {
			ls = append(ls, k)
		}
		sort.Strings(ls)
		r.Fail = append(r.Fail, "请求持有行级以上的锁(不同的键的请求也会互相排队): "+strings.Join(ls, "; "))
	}
	if waitPct > 1 && waits >= 3 {
		var ls []string
		for k, v := range waitBy {
			ls = append(ls, fmt.Sprintf("%s ×%d", k, v))
		}
		sort.Strings(ls)
		r.Fail = append(r.Fail, fmt.Sprintf("不同的键的请求在等锁: %.1f%% 的样本(%s)", waitPct, strings.Join(ls, ", ")))
	}
	if ratio < opt.MinScale {
		r.Fail = append(r.Fail, fmt.Sprintf("%d 路的吞吐只有 1 路的 %.2f 倍(< %.1f): 请求被串行化了", n, ratio, opt.MinScale))
	}
	return r
}
