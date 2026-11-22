package accept

// 命令(命令工厂,R3-1)的验收: 人写的用例(带 之后 的状态)和自动生成的用例。
// 和查询不同,用例会改数据: 每个用例都从 fixtures 的状态开始(每次重新灌),请求之后比较响应 + 相关的表的状态。
// 状态差分测试在 command_diff.go。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"apifactory/factory/command"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// runCommand: 一个命令 API 的全部验收(人写的用例 → 自动生成的用例 → 状态差分测试)。
func runCommand(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API, s *spec.Spec) ([]Result, error) {
	if api.Command.IsHandler() { // 消息处理器(R4): 进程内投递(message.go)
		return runHandler(ctx, d, url, plan, api, s)
	}
	var results []Result
	tables, err := commandTables(ctx, d.main, api, s.Acceptance.Fixtures, s.Acceptance.Cases)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.ID(), err)
	}
	var msg *msgEnv
	if len(api.Command.Emits) > 0 { // 发出(R4): 用例比较 outbox
		msg = newMsgEnv(ctx, d, url, api)
	}
	for _, c := range s.Acceptance.Cases {
		if msg != nil {
			if err := msg.reset(); err != nil {
				return nil, err
			}
		}
		ok, detail := runCommandCase(ctx, d, url, plan, api, tables, s.Acceptance.Fixtures, c)
		if ok && msg != nil && c.Emitted != nil {
			rows, err := msg.newRows()
			if err != nil {
				return nil, err
			}
			if p := emittedDiff(*c.Emitted, rows); len(p) > 0 {
				ok, detail = false, detail+" | "+strings.Join(p, "; ")
			} else {
				detail += " | 发出 ✓"
			}
		}
		results = append(results, Result{Spec: s.ID(), Case: c.Name, Passed: ok, Detail: detail})
	}
	auto, err := spec.LoadAutoAcceptance(s.ID())
	if err != nil {
		return nil, err
	}
	if auto != nil {
		for _, suite := range auto.Suites {
			fx := s.Acceptance.Fixtures
			if suite.Fixtures != nil {
				fx = suite.Fixtures
			}
			for _, c := range suite.Cases {
				if err := loadFixtures(ctx, d, plan, fx); err != nil { // 每个用例从同一个状态开始(上一个用例可能写了数据)
					return nil, fmt.Errorf("%s auto %s fixtures: %w", s.ID(), suite.Name, err)
				}
				ok, detail := runCase(url, c, &api.Response)
				results = append(results, Result{Spec: s.ID(), Suite: "auto:" + suite.Name, Case: c.Name, Passed: ok, Detail: detail})
			}
		}
	}
	if api.Command.Op == "command" { // 组合命令(R3-2): 状态差分 + 故障注入 + 并发 + 重放(command_prog.go)
		results = append(results, runProg(ctx, d, url, plan, api, true)...)
		return append(results, runMQ(ctx, d, url, plan, api, s)...), nil // 发出: 真实 RabbitMQ(FACTORY_TEST_AMQP,mq.go)
	}
	results = append(results, runCommandDiff(ctx, d, url, plan, api)...)
	return append(results, runProg(ctx, d, url, plan, api, false)...), nil // R3-1 的命令: 故障注入 + 并发
}

// commandTables: 用例要看的表 = 命令写的表 + 准备数据 / 之后 里出现的表。
func commandTables(ctx context.Context, conn *pgx.Conn, api *ir.API, fixtures map[string][]map[string]any, cases []spec.Case) (map[string]*command.Table, error) {
	names := map[string]bool{}
	if api.Command.Table != "" {
		names[api.Command.Table] = true
	}
	for _, op := range api.Command.Ops { // 组合命令: 写的每张表
		names[op.Table] = true
	}
	for t := range fixtures {
		names[t] = true
	}
	for _, c := range cases {
		if c.After != nil {
			for t := range c.After.Tables {
				names[t] = true
			}
		}
	}
	out := map[string]*command.Table{}
	for n := range names {
		t, err := command.LoadTable(ctx, conn, n)
		if err != nil {
			return nil, err
		}
		out[n] = t
	}
	return out, nil
}

// runCommandCase: 灌 fixtures → 记下之前的状态 → 请求 → 比较响应 → 比较之后的状态。
func runCommandCase(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API, tables map[string]*command.Table,
	fixtures map[string][]map[string]any, c spec.Case) (bool, string) {
	if err := loadFixtures(ctx, d, plan, fixtures); err != nil {
		return false, "fixtures: " + err.Error()
	}
	before := map[string][]command.Row{}
	for n, t := range tables {
		rows, err := command.ReadRows(ctx, d.main, t)
		if err != nil {
			return false, err.Error()
		}
		before[n] = rows
	}
	ok, detail := runCase(url, c, &api.Response)
	if !ok {
		return false, detail
	}
	if c.After == nil {
		return false, "命令的用例没有 after(之后)"
	}
	names := make([]string, 0, len(tables))
	for n := range tables {
		names = append(names, n)
	}
	sort.Strings(names)
	var diffs []string
	for _, n := range names {
		t := tables[n]
		got, err := command.ReadRows(ctx, d.main, t)
		if err != nil {
			return false, err.Error()
		}
		want, listed := c.After.Tables[n]
		if c.After.Unchanged || !listed { // 没写出的表: 和请求之前一样
			if msg := command.Diff(t, before[n], got, false); msg != "" {
				diffs = append(diffs, "应该不变: "+msg)
			}
			continue
		}
		if msg := afterDiff(plan, t, want, got); msg != "" {
			diffs = append(diffs, msg)
		}
	}
	if len(diffs) > 0 {
		return false, detail + " | 之后的状态不对: " + strings.Join(diffs, "; ")
	}
	return true, detail + " | 之后的状态 ✓"
}

// afterDiff: 之后 写出的行(全部的行,按主键对应,只比较写出来的列)vs 数据库。
func afterDiff(plan *ir.Plan, t *command.Table, want []map[string]any, got []command.Row) string {
	byKey := map[string]command.Row{}
	for _, r := range got {
		byKey[t.KeyOf(r)] = r
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range want {
		cw := command.Row{}
		for col, v := range w {
			c := t.Col(col)
			if c == nil {
				return fmt.Sprintf("之后 %s: 没有列 %s", t.Name, col)
			}
			if v == "$absent" { // 之后 的 不设置 = 空(NULL)
				v = nil
			}
			dv, err := fixtureValue(plan, t.Name, col, v)
			if err != nil {
				return err.Error()
			}
			cv, err := command.Canon(c.Type, dv)
			if err != nil {
				return fmt.Sprintf("之后 %s.%s: %v", t.Name, col, err)
			}
			cw[col] = cv
		}
		k := t.KeyOf(cw)
		seen[k] = true
		g, ok := byKey[k]
		if !ok {
			out = append(out, fmt.Sprintf("%s[%s]: 应该有,数据库里没有", t.Name, k))
			continue
		}
		cols := make([]string, 0, len(cw))
		for col := range cw {
			cols = append(cols, col)
		}
		sort.Strings(cols)
		for _, col := range cols {
			if !command.Equal(cw[col], g[col]) {
				out = append(out, fmt.Sprintf("%s[%s].%s: 应该是 %s,数据库是 %s", t.Name, k, col, command.Show(cw[col]), command.Show(g[col])))
			}
		}
	}
	for _, g := range got {
		if k := t.KeyOf(g); !seen[k] {
			out = append(out, fmt.Sprintf("%s[%s]: 数据库里多了这一行", t.Name, k))
		}
	}
	return strings.Join(out, "; ")
}

// callJSON: 发一个请求(Connect 的 HTTP/JSON)。返回错误码(成功 = OK)、错误消息、响应(proto 的字段名)。
func callJSON(url string, reqBody map[string]any, out *ir.Message) (code, msg string, got map[string]any, err error) {
	now, _ := reqBody[NowKey].(string)
	body, _ := json.Marshal(withoutNow(reqBody))
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if now != "" {
		req.Header.Set(NowHeader, now)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if t := os.Getenv("API_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return strings.ToUpper(e.Code), e.Message, nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded map[string]any
	if err := dec.Decode(&decoded); err != nil {
		return "", "", nil, fmt.Errorf("bad json: %s", raw)
	}
	got, _ = protoKeys(decoded, out).(map[string]any)
	return "OK", "", got, nil
}

// validator: 请求按 proto 的规则(protovalidate,和服务器同一套)是否不合法。
type validator struct {
	md protoreflect.MessageDescriptor
}

func (v validator) invalid(req map[string]any) (bool, error) {
	b, err := json.Marshal(withoutNow(req))
	if err != nil {
		return false, err
	}
	msg := dynamicpb.NewMessage(v.md)
	if err := protojson.Unmarshal(b, msg); err != nil {
		return false, err
	}
	err = protovalidate.Validate(msg)
	var verr *protovalidate.ValidationError
	if errors.As(err, &verr) {
		return true, nil
	}
	return false, err
}
