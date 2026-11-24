package perf

// 单键探针(docs/design/hotspots.md §6,⚖️ H6): 按键更新的 API,N 路都打同一个键 → "同一键约 N/s"。
// 只报告,不失败: 单键的上限是物理的(行锁: ≈ 1 / 一次事务的时间),只能告诉人(同时抢: 是 → 分桶)。

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MIERAI/api-factory/runtime/txn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// updatesByKey: 请求里有按键的更新(状态机的转移也算)。
func updatesByKey(ws writeSpec) bool {
	for _, op := range ws.ops {
		if op.Kind == "update" && len(op.Key) > 0 {
			return true
		}
	}
	return false
}

func singleKeyProbe(ctx context.Context, conn *pgx.Conn, ws writeSpec, tables map[string]*table, opt Options, seq map[string]int) Report {
	n, m := max(opt.Workers, 2), opt.WriteRequests
	r := Report{API: ws.api.ID, Variant: fmt.Sprintf("%s: 同一键 %d 路(信息)", ws.variant, n)}
	info := func(f string, a ...any) Report {
		r.Info = append(r.Info, fmt.Sprintf(f, a...))
		return r
	}
	hot, _ := ParseSkew("hot:1")
	envs, err := genRequests(ctx, conn, ws, tables, n*m, seq, hot)
	if err != nil {
		return info("单键探针: 造不出请求(%v)", err)
	}
	cfg, err := pgxpool.ParseConfig(opt.DB)
	if err != nil {
		return info("%v", err)
	}
	cfg.MaxConns = int32(n)
	cfg.ConnConfig.RuntimeParams["TimeZone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return info("%v", err)
	}
	defer pool.Close()
	ex := txn.New(pool)
	var okN, bizN, errN atomic.Int64
	t0 := time.Now()
	var wg sync.WaitGroup
	for w := range n {
		wg.Add(1)
		go func(reqs []*reqEnv) {
			defer wg.Done()
			for _, e := range reqs {
				err := ex.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error { return runRequest(ctx, tx, ws, e, nil) })
				switch {
				case err == nil:
					okN.Add(1)
				case business(err):
					bizN.Add(1)
				default:
					errN.Add(1)
				}
			}
		}(envs[w*m : (w+1)*m])
	}
	wg.Wait()
	sec := time.Since(t0).Seconds()
	r.Ms = 1000 * sec / float64(n*m)
	r.Rows = int(okN.Load())
	r.Plan = fmt.Sprintf("同一键约 %.0f 请求/秒(%d 路 × %d;成功 %d / 业务结果 %d / 错误 %d)", float64(n*m)/sec, n, m, okN.Load(), bizN.Load(), errN.Load())
	return info("同一键的吞吐上限约 %.0f/s(共享数量的读改写: 同时抢 = 是 → 分桶的生产线;docs/design/hotspots.md)", float64(n*m)/sec)
}
