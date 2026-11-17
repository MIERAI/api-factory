// Package accept 是验收执行器(黑盒): 照着式样书的 acceptance 节和自动生成的用例执行,与 AI 无关。
//
// 输入只有: 被测 API 的 URL、灌 fixture 用的 DB、IR(字段的 presence 等)。
// 不启动服务器、不调用任何语言的工具链 —— 只要服务说 Connect 协议(HTTP/JSON),谁做的都能验收。
// 服务器的启动(以及"关掉索引扫描"这种测试环境设置)由编排层负责(factory serve-test)。
package accept

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/ir"
	"apifactory/factory/sources"
	"apifactory/factory/spec"
)

type Result struct {
	Spec   string
	Suite  string // 空 = 人写的用例;auto:* = 自动生成
	Case   string
	Passed bool
	Detail string
}

// Run 对每份式样书灌入 fixture 并执行用例。baseURL 是被测服务器,fixturesDB 只用来灌数据。
func Run(ctx context.Context, baseURL, fixturesDB string, plan *ir.Plan, specs []*spec.Spec) ([]Result, error) {
	conn, err := connect(ctx, fixturesDB)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	defer conn.Close(ctx)
	d, err := openDBs(ctx, conn)
	if err != nil {
		return nil, err
	}
	defer d.close()

	apis := map[string]*ir.API{}
	for i := range plan.APIs {
		apis[plan.APIs[i].ID] = &plan.APIs[i]
	}

	var results []Result
	for _, s := range specs {
		api, ok := apis[s.ID()]
		if !ok {
			return nil, fmt.Errorf("%s: not in the IR (run factory prepare first)", s.ID())
		}
		if err := loadFixtures(ctx, d, plan, s.Acceptance.Fixtures); err != nil {
			return nil, fmt.Errorf("%s fixtures: %w", s.ID(), err)
		}
		url := fmt.Sprintf("%s/%s.%s/%s", strings.TrimRight(baseURL, "/"), api.ProtoPackage, api.Service, api.Method)
		out := &api.Response
		for _, c := range s.Acceptance.Cases {
			ok, detail := runCase(url, c, out)
			results = append(results, Result{Spec: s.ID(), Case: c.Name, Passed: ok, Detail: detail})
		}
		// 自动生成的机制测试(gen/acceptance/<id>.yaml)
		auto, err := spec.LoadAutoAcceptance(s.ID())
		if err != nil {
			return nil, err
		}
		if auto != nil {
			for _, suite := range auto.Suites {
				if suite.Fixtures != nil {
					if err := loadFixtures(ctx, d, plan, suite.Fixtures); err != nil {
						return nil, fmt.Errorf("%s auto %s fixtures: %w", s.ID(), suite.Name, err)
					}
				}
				for _, c := range suite.Cases {
					ok, detail := runCase(url, c, out)
					results = append(results, Result{Spec: s.ID(), Suite: "auto:" + suite.Name, Case: c.Name, Passed: ok, Detail: detail})
				}
			}
		}
		// 差分测试(随机数据 + 参考求值器)
		results = append(results, runDiff(ctx, d, url, plan, api)...)
	}
	return results, nil
}

// loadFixtures: enum 列可以写 proto 的值名(JOB_STATUS_SUCCEEDED),按领域层的映射转换成 DB 值。
// fixture 和期待值用同一种表示,避免"DB 值和 proto 编号"混淆(第一波实测: 本地模型审查的误报大多来自这里)。
func loadFixtures(ctx context.Context, d *dbs, plan *ir.Plan, fixtures map[string][]map[string]any) error {
	tables := make([]string, 0, len(fixtures))
	for t := range fixtures {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, table := range tables {
		if st := plan.Storage(table); st != nil {
			if err := loadStorageRows(ctx, d, plan, st, fixtures[table]); err != nil {
				return fmt.Errorf("fixture %s: %w", table, err)
			}
			continue
		}
		ident := pgx.Identifier{table}.Sanitize()
		sh := plan.Shard(table)
		targets, err := d.targets(sh)
		if err != nil {
			return err
		}
		for _, c := range targets {
			if _, err := c.Exec(ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
				return err
			}
		}
		for _, row := range fixtures[table] {
			conn := d.main
			if sh != nil { // 分库: 写进分库键对应的库
				if conn, err = d.forKey(row[sh.Column]); err != nil {
					return fmt.Errorf("fixture %s: %w", table, err)
				}
			}
			cols := make([]string, 0, len(row))
			for c := range row {
				cols = append(cols, c)
			}
			sort.Strings(cols)
			idents, holders, args := make([]string, len(cols)), make([]string, len(cols)), make([]any, len(cols))
			for i, c := range cols {
				idents[i] = pgx.Identifier{c}.Sanitize()
				holders[i] = "$" + strconv.Itoa(i+1)
				v, err := fixtureValue(plan, table, c, row[c])
				if err != nil {
					return fmt.Errorf("fixture %s: %w", table, err)
				}
				args[i] = v
			}
			sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(idents, ","), strings.Join(holders, ","))
			if _, err := conn.Exec(ctx, sql, args...); err != nil {
				return fmt.Errorf("insert %v: %w", row, err)
			}
		}
	}
	return nil
}

func runCase(url string, c spec.Case, out *ir.Message) (bool, string) {
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

func runStep(url string, reqBody map[string]any, expect spec.Expect, out *ir.Message) (bool, string, map[string]any) {
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
	// "!CODE": 只要求不是这个 code(用于"合法输入不能被校验拒绝",结果可能是 OK 也可能是 NOT_FOUND)
	if not, ok := strings.CutPrefix(wantCode, "!"); ok {
		gotCode := "OK"
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal(raw, &e)
			gotCode = strings.ToUpper(e.Code)
		}
		if gotCode == not {
			return false, fmt.Sprintf("want not %s, got %s: %s", not, gotCode, truncate(raw, 200)), nil
		}
		return true, "not " + not + " (" + gotCode + ")", nil
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		gotCode := strings.ToUpper(e.Code)
		if gotCode == wantCode {
			if expect.MessageContains != "" && !strings.Contains(e.Message, expect.MessageContains) {
				return false, fmt.Sprintf("%s for the wrong reason: message %q does not contain %q", gotCode, e.Message, expect.MessageContains), nil
			}
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
	got, _ := protoKeys(decoded, out).(map[string]any)
	for k, want := range expect.Response {
		if msg := match("response."+k, want, got[k], out.Field(k)); msg != "" {
			return false, fmt.Sprintf("%s (body %s)", msg, truncate(raw, 300)), got
		}
	}
	return true, "OK " + truncate(raw, 120), got
}

// match 比对期待值与实际值,不一致时返回说明。fd 是该字段的 proto 描述符(未知时为 nil)。
//
//   - "$absent"   : 字段不在响应 JSON 里(未设置;非 optional 字段的零值也会被 protojson 省略)
//   - null        : 没有值(有 presence 的字段 = 未设置;其他字段 = 零值)
//   - "$nonempty" : 字段存在且非零值
//   - map         : 只比对期待值里写出的键(部分一致)
//   - 数组        : 长度相同,逐个元素比对
//   - 标量        : 吸收 protojson 的特性(非 optional 字段的零值省略、int64 编码为字符串)
//
// 有 presence 的字段(proto3 optional / message): 期待具体值时,"未设置"不算等于零值。
func match(path string, want, got any, fd *ir.Field) string {
	presence := fd != nil && !fd.List && fd.Presence
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
		var md *ir.Message
		if fd != nil {
			md = fd.Message
		}
		for k, wv := range w {
			var sub *ir.Field
			if md != nil {
				sub = md.Field(k)
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
	if want == nil { // 设计稿里写 null = "没有值": 有 presence 的字段 = 未设置;其他字段 = 零值(protojson 省略)
		if presence && got != nil {
			return fmt.Sprintf("%s: want unset (null), got %v", path, got)
		}
		if !presence && !isZero(got) {
			return fmt.Sprintf("%s: want null / zero, got %v", path, got)
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

// protoKeys: protojson 的键(lowerCamelCase)→ proto 的字段名,按消息的字段定义对照(protoc 的 JSON 名规则)。
// 只靠蛇形转换有歧义: l_column_1 的 JSON 名是 lColumn1,转回来是 l_column1(试用的探针里发现)。
// 不在定义里的键退回 snakeKeys。
func protoKeys(v any, md *ir.Message) any {
	switch x := v.(type) {
	case map[string]any:
		if md == nil {
			return snakeKeys(x)
		}
		byJSON := map[string]*ir.Field{}
		for i := range md.Fields {
			byJSON[jsonName(md.Fields[i].Name)] = &md.Fields[i]
		}
		out := make(map[string]any, len(x))
		for k, val := range x {
			if f, ok := byJSON[k]; ok {
				out[f.Name] = protoKeys(val, f.Message)
			} else {
				out[toSnake(k)] = snakeKeys(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = protoKeys(val, md)
		}
		return out
	}
	return v
}

// jsonName: protoc 的 JSON 名(去掉 _,后面的小写字母变大写;数字不变)。
func jsonName(name string) string {
	var b strings.Builder
	up := false
	for _, r := range name {
		if r == '_' {
			up = true
			continue
		}
		if up && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		up = false
		b.WriteRune(r)
	}
	return b.String()
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

// fixtureValue: enum 列写了值名时转换成 DB 值;写数字时原样使用。
func fixtureValue(plan *ir.Plan, table, column string, v any) (any, error) {
	name, ok := v.(string)
	if !ok {
		return v, nil
	}
	e := plan.Enum(table, column)
	if e == nil {
		return v, nil
	}
	for _, ev := range e.Values {
		if ev.Name == name {
			return ev.DB, nil
		}
	}
	return nil, fmt.Errorf("%s.%s: %q is not a value of %s (domain layer maps: %v)", table, column, name, e.ProtoEnum, enumNames(e))
}

func enumNames(e *ir.Enum) []string {
	var out []string
	for _, v := range e.Values {
		out = append(out, v.Name)
	}
	return out
}

// loadStorageRows: 逻辑表(存储: JSON版本)的数据行 → 物理表的行。
// 行的顺序 = 登记的顺序: 登记顺序列按类型合成单调递增的值(时间 +1 秒 / 整数 +1)。
// "$action": "delete" → 操作列 = 删除值;"logical_delete" → JSON 里加上逻辑删除的键。
func loadStorageRows(ctx context.Context, d *dbs, plan *ir.Plan, st *ir.Storage, rows []map[string]any) error {
	ident := pgx.Identifier{st.Table}.Sanitize()
	sh := plan.Shard(st.View)
	targets, err := d.targets(sh)
	if err != nil {
		return err
	}
	for _, c := range targets {
		if _, err := c.Exec(ctx, "TRUNCATE "+ident); err != nil {
			return err
		}
	}
	conn := d.main
	orderTypes := map[string]string{}
	for _, c := range st.OrderBy {
		var t string
		err := conn.QueryRow(ctx, `SELECT data_type FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, st.Table, c).Scan(&t)
		if err != nil {
			return fmt.Errorf("登记顺序列 %s.%s: %w", st.Table, c, err)
		}
		orderTypes[c] = t
	}
	src, ok := sources.ByKind(st.Kind) // 逻辑表的一行 → 物理表的一行: 由来源决定
	if !ok || src.Row == nil {
		return fmt.Errorf("%s: 存储 %s 不存在", st.View, st.Kind)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, row := range rows {
		ph, err := src.Row(st, row, func(col string, v any) (any, error) { return fixtureValue(plan, st.View, col, v) })
		if err != nil {
			return err
		}
		var cols []string
		var args []any
		names := make([]string, 0, len(ph.Values))
		for c := range ph.Values {
			names = append(names, c)
		}
		sort.Strings(names)
		for _, c := range names {
			cols = append(cols, pgx.Identifier{c}.Sanitize())
			args = append(args, ph.Values[c])
		}
		for _, c := range st.OrderBy { // 行的顺序 = 登记的顺序
			if !ph.Order {
				break
			}
			if _, given := ph.Values[c]; given { // 例子里写了(外壳列: 列: registered_at)→ 用例子的值
				continue
			}
			var v any
			switch orderTypes[c] {
			case "timestamp with time zone", "timestamp without time zone":
				v = base.Add(time.Duration(i) * time.Second)
			case "bigint", "integer", "smallint":
				v = int64(i + 1)
			default:
				return fmt.Errorf("登记顺序列 %s.%s 的类型 %s 不支持合成", st.Table, c, orderTypes[c])
			}
			cols = append(cols, pgx.Identifier{c}.Sanitize())
			args = append(args, v)
		}
		holders := make([]string, len(cols))
		for j := range cols {
			holders[j] = "$" + strconv.Itoa(j+1)
		}
		sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(cols, ","), strings.Join(holders, ","))
		target := conn
		if sh != nil { // 分库: 按逻辑行的分库键
			if target, err = d.forKey(row[sh.Column]); err != nil {
				return err
			}
		}
		if _, err := target.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf("insert %v: %w", row, err)
		}
	}
	return nil
}
