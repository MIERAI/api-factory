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
	"google.golang.org/protobuf/reflect/protoreflect"

	"apifactory/factory/protoload"
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

	files, err := protoload.Load()
	if err != nil {
		return nil, err
	}

	var results []Result
	for _, s := range specs {
		if err := loadFixtures(ctx, conn, s.Acceptance.Fixtures); err != nil {
			return nil, fmt.Errorf("%s fixtures: %w", s.ID(), err)
		}
		parts, err := s.RPCParts()
		if err != nil {
			return nil, err
		}
		md, err := protoload.FindMethod(files, parts.ProtoPackage, parts.Service, parts.Method)
		if err != nil {
			return nil, err
		}
		url := fmt.Sprintf("%s/%s.%s/%s", base, parts.ProtoPackage, parts.Service, parts.Method)
		for _, c := range s.Acceptance.Cases {
			ok, detail := runCase(url, c, md.Output())
			results = append(results, Result{Spec: s.ID(), Case: c.Name, Passed: ok, Detail: detail})
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

func runCase(url string, c spec.Case, out protoreflect.MessageDescriptor) (bool, string) {
	var prev map[string]any
	steps := c.AllSteps()
	var details []string
	for i, st := range steps {
		req, err := substitute(st.Request, prev)
		if err != nil {
			return false, fmt.Sprintf("step %d: %v", i+1, err)
		}
		ok, detail, got := runStep(url, req, st.Expect, out)
		if len(steps) > 1 {
			detail = fmt.Sprintf("step %d: %s", i+1, detail)
		}
		details = append(details, detail)
		if !ok {
			return false, strings.Join(details, " | ")
		}
		prev = got
	}
	return true, strings.Join(details, " | ")
}

// substitute 把 "$prev.<field>" 替换成上一步响应里的值。
func substitute(req map[string]any, prev map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range req {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "$prev.") {
			field := strings.TrimPrefix(s, "$prev.")
			val, ok := prev[field]
			if !ok {
				return nil, fmt.Errorf("%s: previous response has no %q", k, field)
			}
			v = val
		}
		out[k] = v
	}
	return out, nil
}

func runStep(url string, reqBody map[string]any, expect spec.Expect, out protoreflect.MessageDescriptor) (bool, string, map[string]any) {
	body, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err.Error(), nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	wantCode := strings.ToUpper(expect.Code)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		gotCode := strings.ToUpper(e.Code)
		if gotCode == wantCode {
			return true, fmt.Sprintf("%s (%s)", gotCode, e.Message), nil
		}
		return false, fmt.Sprintf("want %s, got %s: %s", wantCode, gotCode, e.Message), nil
	}
	if wantCode != "OK" {
		return false, fmt.Sprintf("want %s, got OK: %s", wantCode, raw), nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false, fmt.Sprintf("bad json: %s", raw), nil
	}
	got, _ := snakeKeys(decoded).(map[string]any)
	for k, want := range expect.Response {
		if msg := match("response."+k, want, got[k], out.Fields().ByName(protoreflect.Name(k))); msg != "" {
			return false, fmt.Sprintf("%s (body %s)", msg, truncate(raw, 300)), got
		}
	}
	return true, "OK " + truncate(raw, 120), got
}

// match 比对期待值与实际值,不一致时返回说明。fd 是该字段的 proto 描述符(未知时为 nil)。
//
//   - "$absent"   : 字段不在响应 JSON 里(未设置;非 optional 字段的零值也会被 protojson 省略)
//   - "$nonempty" : 字段存在且非零值
//   - map         : 只比对期待值里写出的键(部分一致)
//   - 数组        : 长度相同,逐个元素比对
//   - 标量        : 吸收 protojson 的特性(非 optional 字段的零值省略、int64 编码为字符串)
//
// 有 presence 的字段(proto3 optional / message): 期待具体值时,"未设置"不算等于零值。
func match(path string, want, got any, fd protoreflect.FieldDescriptor) string {
	presence := fd != nil && !fd.IsList() && fd.HasPresence()
	switch w := want.(type) {
	case string:
		switch w {
		case "$absent":
			if got != nil {
				return fmt.Sprintf("%s: want absent (unset), got %v", path, got)
			}
			return ""
		case "$nonempty":
			if isZero(got) {
				return fmt.Sprintf("%s: want non-empty, got %v", path, got)
			}
			return ""
		}
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			if got == nil && len(w) == 0 {
				return ""
			}
			return fmt.Sprintf("%s: want object, got %v", path, got)
		}
		var md protoreflect.MessageDescriptor
		if fd != nil {
			md = fd.Message()
		}
		for k, wv := range w {
			var sub protoreflect.FieldDescriptor
			if md != nil {
				sub = md.Fields().ByName(protoreflect.Name(k))
			}
			if msg := match(path+"."+k, wv, g[k], sub); msg != "" {
				return msg
			}
		}
		return ""
	case []any:
		var g []any
		if got != nil {
			var ok bool
			if g, ok = got.([]any); !ok {
				return fmt.Sprintf("%s: want array, got %v", path, got)
			}
		}
		if len(g) != len(w) {
			return fmt.Sprintf("%s: want %d item(s), got %d", path, len(w), len(g))
		}
		for i := range w {
			if msg := match(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], fd); msg != "" {
				return msg
			}
		}
		return ""
	}
	if presence && got == nil {
		return fmt.Sprintf("%s: want %v, got unset (field has presence: unset != zero)", path, want)
	}
	if !valueEqual(want, got) {
		return fmt.Sprintf("%s: want %v, got %v", path, want, got)
	}
	return ""
}

// snakeKeys 把 protojson 输出的 lowerCamelCase 键(nextPageToken)转回 proto 字段名(next_page_token),
// 这样式样书可以统一用 proto 里的字段名书写。
func snakeKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[toSnake(k)] = snakeKeys(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = snakeKeys(val)
		}
		return out
	}
	return v
}

func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
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
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
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
