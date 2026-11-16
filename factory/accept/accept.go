// Package accept 是验收执行器: 照着式样书的 acceptance 节执行,与 AI 无关。
//
// 通过 Connect 协议的 HTTP/JSON 调用 API,所以和运行时用什么语言无关:
// 只要服务说 Connect(或经过网关转换),同一个执行器就能验收。
package accept

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/spec"
)

type Result struct {
	Spec   string
	Case   string
	Passed bool
	Detail string
}

// Run 启动服务器二进制,对每份式样书灌入 fixture 并执行用例。
func Run(ctx context.Context, serverBin, databaseURL string, specs []*spec.Spec) ([]Result, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	srv := exec.CommandContext(ctx, serverBin)
	srv.Env = append(os.Environ(), "PORT="+strconv.Itoa(port), "DATABASE_URL="+databaseURL)
	logFile, err := os.Create(".factory/server.log")
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	srv.Stdout, srv.Stderr = logFile, logFile
	if err := srv.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	defer func() { _ = srv.Process.Kill(); _ = srv.Wait() }()
	if err := waitHealthy(base, 15*time.Second); err != nil {
		return nil, fmt.Errorf("%w (see .factory/server.log)", err)
	}

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	defer conn.Close(ctx)

	var results []Result
	for _, s := range specs {
		if err := loadFixtures(ctx, conn, s.Acceptance.Fixtures); err != nil {
			return nil, fmt.Errorf("%s fixtures: %w", s.API.ID, err)
		}
		parts, err := s.RPCParts()
		if err != nil {
			return nil, err
		}
		url := fmt.Sprintf("%s/%s.%s/%s", base, parts.ProtoPackage, parts.Service, parts.Method)
		for _, c := range s.Acceptance.Cases {
			ok, detail := runCase(url, c)
			results = append(results, Result{Spec: s.API.ID, Case: c.Name, Passed: ok, Detail: detail})
		}
	}
	return results, nil
}

func loadFixtures(ctx context.Context, conn *pgx.Conn, fixtures map[string][]map[string]any) error {
	tables := make([]string, 0, len(fixtures))
	for t := range fixtures {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, table := range tables {
		ident := pgx.Identifier{table}.Sanitize()
		if _, err := conn.Exec(ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
			return err
		}
		for _, row := range fixtures[table] {
			cols := make([]string, 0, len(row))
			for c := range row {
				cols = append(cols, c)
			}
			sort.Strings(cols)
			idents, holders, args := make([]string, len(cols)), make([]string, len(cols)), make([]any, len(cols))
			for i, c := range cols {
				idents[i] = pgx.Identifier{c}.Sanitize()
				holders[i] = "$" + strconv.Itoa(i+1)
				args[i] = row[c]
			}
			sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(idents, ","), strings.Join(holders, ","))
			if _, err := conn.Exec(ctx, sql, args...); err != nil {
				return fmt.Errorf("insert %v: %w", row, err)
			}
		}
	}
	return nil
}

func runCase(url string, c spec.Case) (bool, string) {
	reqBody := c.Request
	if reqBody == nil {
		reqBody = map[string]any{}
	}
	body, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	wantCode := strings.ToUpper(c.Expect.Code)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		gotCode := strings.ToUpper(e.Code)
		if gotCode == wantCode {
			return true, fmt.Sprintf("%s (%s)", gotCode, e.Message)
		}
		return false, fmt.Sprintf("want %s, got %s: %s", wantCode, gotCode, e.Message)
	}
	if wantCode != "OK" {
		return false, fmt.Sprintf("want %s, got OK: %s", wantCode, raw)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false, fmt.Sprintf("bad json: %s", raw)
	}
	for k, want := range c.Expect.Response {
		if !valueEqual(want, got[k]) {
			return false, fmt.Sprintf("response.%s: want %v, got %v (body %s)", k, want, got[k], raw)
		}
	}
	return true, fmt.Sprintf("OK %s", raw)
}

// valueEqual 吸收 protojson 的两个特性:
//   - 零值字段会被省略(status: 0 → {})
//   - int64 会被编码成字符串("3000000000")
func valueEqual(want, got any) bool {
	if got == nil {
		return isZero(want)
	}
	wn, wok := toFloat(want)
	gn, gok := toFloat(got)
	if wok && gok {
		return wn == gn
	}
	return fmt.Sprint(want) == fmt.Sprint(got)
}

func isZero(v any) bool {
	if n, ok := toFloat(v); ok {
		return n == 0
	}
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
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
