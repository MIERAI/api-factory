// Package testenv: 测试环境的编排(启动被测服务器 → 执行命令 → 停止)。验收执行器本身不管服务器。
package testenv

import (
	"context"
	"fmt"
	"io"
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
//
// 并行跑时的两个竞争(试用的并行验收里实际发生): ① freePort 拿到端口到服务器绑定之间,别的进程可能拿到同一个端口 →
// 我们的服务器绑定失败退出,而健康检查被别人的服务器回答(请求会发到别人的库!)② 日志文件共用。
// 所以: 服务器进程退出了就换端口重试;健康之后确认进程还活着;日志按端口分开。
func WithServer(ctx context.Context, serverBin, databaseURL, logPath string, cmd []string) error {
	env := append(os.Environ(), "DATABASE_URL="+PlanAgnostic(databaseURL))
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
		env = append(env, "FACTORY_SHARDS="+strings.Join(parts, ";"))
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		port, err := freePort()
		if err != nil {
			return err
		}
		base := "http://127.0.0.1:" + strconv.Itoa(port)
		lp := strings.TrimSuffix(logPath, ".log") + "-" + strconv.Itoa(port) + ".log"
		logFile, err := os.Create(lp)
		if err != nil {
			return err
		}
		srv := exec.CommandContext(ctx, serverBin)
		id := fmt.Sprintf("%d-%d-%d", os.Getpid(), port, time.Now().UnixNano())
		srv.Env = append(append([]string(nil), env...), "PORT="+strconv.Itoa(port), "INSTANCE_ID="+id)
		srv.Stdout, srv.Stderr = logFile, logFile
		if err := srv.Start(); err != nil {
			logFile.Close()
			return fmt.Errorf("start server: %w", err)
		}
		exited := make(chan struct{})
		go func() { _ = srv.Wait(); close(exited) }()
		stop := func() { _ = srv.Process.Kill(); <-exited; logFile.Close() }
		err = waitHealthy(base, id, 60*time.Second, exited) // 并行跑时机器很忙,15 秒曾经不够
		select {
		case <-exited: // 我们的服务器退出了(端口被占用等): 健康检查可能是别人回答的 → 换端口
			logFile.Close()
			lastErr = fmt.Errorf("server exited before ready (see %s)", lp)
			continue
		default:
		}
		if err != nil {
			stop()
			return fmt.Errorf("%w (see %s)", err, lp)
		}
		c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
		c.Env = append(os.Environ(), "BASE_URL="+base)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		runErr := c.Run()
		stop()
		_ = os.Remove(lp)
		return runErr
	}
	return lastErr
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthy: 健康检查要由这个进程回答(body 里的实例标识一致)。
func waitHealthy(base, id string, timeout time.Duration, exited <-chan struct{}) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return fmt.Errorf("server exited")
		default:
		}
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == "ok "+id {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("server not healthy within %s", timeout)
}
