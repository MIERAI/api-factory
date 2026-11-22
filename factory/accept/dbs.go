package accept

// 验收用的数据库: 主库 + 分库(FACTORY_SHARDS,和被测服务器同一份配置)。
// 分库的表: 每一行写进分库键对应的库;清空 / 读回时针对所有的库。

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/ir"
	"github.com/MIERAI/api-factory-runtime/shard"
)

type dbs struct {
	ctx   context.Context
	main  *pgx.Conn
	rules []shard.Rule
	conns map[string]*pgx.Conn
}

func openDBs(ctx context.Context, main *pgx.Conn) (*dbs, error) {
	rules, err := shard.Parse(os.Getenv("FACTORY_SHARDS"))
	if err != nil {
		return nil, err
	}
	return &dbs{ctx: ctx, main: main, rules: rules, conns: map[string]*pgx.Conn{}}, nil
}

func (d *dbs) close() {
	for _, c := range d.conns {
		_ = c.Close(d.ctx)
	}
}

func (d *dbs) conn(url string) (*pgx.Conn, error) {
	if url == "" {
		return d.main, nil
	}
	if c, ok := d.conns[url]; ok {
		return c, nil
	}
	c, err := connect(d.ctx, url)
	if err != nil {
		return nil, fmt.Errorf("shard %s: %w", url, err)
	}
	if _, err := c.Exec(d.ctx, "SET TimeZone = 'UTC'"); err != nil {
		return nil, err
	}
	d.conns[url] = c
	return c, nil
}

// forKey: 分库键的值 → 连接(和服务器的 shard.Pick 一致)。
func (d *dbs) forKey(v any) (*pgx.Conn, error) {
	var key int64
	switch x := v.(type) {
	case int64:
		key = x
	case int:
		key = int64(x)
	case float64:
		key = int64(x)
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("分库键 %q 不是整数", x)
		}
		key = n
	case nil:
		return d.main, nil
	default:
		return nil, fmt.Errorf("分库键 %v(%T)不是整数", v, v)
	}
	return d.conn(shard.Pick(d.rules, key))
}

// all: 主库 + 所有分库。
func (d *dbs) all() ([]*pgx.Conn, error) {
	out := []*pgx.Conn{d.main}
	seen := map[string]bool{}
	for _, r := range d.rules {
		if seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		c, err := d.conn(r.URL)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// targets: 表 table 要清空 / 读回的库(分库的表 = 所有的库)。
func (d *dbs) targets(sh *ir.Shard) ([]*pgx.Conn, error) {
	if sh == nil {
		return []*pgx.Conn{d.main}, nil
	}
	return d.all()
}

// connect: 单个连接。URL 里给服务器连接池的参数(pool_max_conns 等)去掉,否则会被当成 Postgres 的运行参数。
func connect(ctx context.Context, raw string) (*pgx.Conn, error) {
	if u, err := neturl.Parse(raw); err == nil {
		q := u.Query()
		for k := range q {
			if strings.HasPrefix(k, "pool_") {
				q.Del(k)
			}
		}
		u.RawQuery = q.Encode()
		raw = u.String()
	}
	return pgx.Connect(ctx, raw)
}
