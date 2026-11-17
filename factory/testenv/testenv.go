// Package testenv: 测试环境的编排(启动被测服务器 → 执行命令 → 停止)。验收执行器本身不管服务器。
package testenv

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"apifactory/platform/shard"
)

// PlanAgnostic 让被测服务器的 DB 会话不使用索引扫描。
// 验收结果不能依赖执行计划: 正确的顺序必须由 SQL 本身保证,而不是碰巧由某个索引保证。
// (实测: 去掉 keyset 的次级排序这种 bug,在用到索引时会被索引顺序掩盖,验收全绿)
func PlanAgnostic(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return databaseURL
	}
	q := u.Query()
	opts := strings.TrimSpace(q.Get("options") + " -c enable_indexscan=off -c enable_indexonlyscan=off -c enable_bitmapscan=off")
	q.Set("options", opts)
	// url.Values.Encode 把空格编码成 "+",pgx 不会还原,必须用 %20
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String()
}

// WithServer 启动服务器,把 BASE_URL 放进环境变量后执行 cmd,结束后停止服务器。
func WithServer(ctx context.Context, serverBin, databaseURL, logPath string, cmd []string) error {
	port, err := freePort()
	if err != nil {
		return err
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	srv := exec.CommandContext(ctx, serverBin)
	srv.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "DATABASE_URL="+PlanAgnostic(databaseURL))
	if spec := os.Getenv("FACTORY_SHARDS"); spec != "" { // 分库也一样不用索引扫描
		rules, err := shard.Parse(spec)
		if err != nil {
			return err
		}
		var parts []string
		for _, r := range rules {
			key := fmt.Sprintf("%d-%d", r.Lo, r.Hi)
			if r.Any {
				key = "*"
			}
			parts = append(parts, key+"="+PlanAgnostic(r.URL))
		}
		srv.Env = append(srv.Env, "FACTORY_SHARDS="+strings.Join(parts, ";"))
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	srv.Stdout, srv.Stderr = logFile, logFile
	if err := srv.Start(); err != nil {
		return fmt.Errorf("start server: %w", err)
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()
	if err := waitHealthy(base, 60*time.Second); err != nil { // 并行跑时机器很忙,15 秒曾经不够
		return fmt.Errorf("%w (see %s)", err, logPath)
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Env = append(os.Environ(), "BASE_URL="+base)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(base string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("server not healthy within %s", timeout)
}
