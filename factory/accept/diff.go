package accept

// 差分测试: 随机数据 + 随机请求,被测服务器的响应 vs 核心代数的参考求值器(IR 里的流水线)。
//
// 数据在运行时生成并插入: 主键 / 唯一列(从数据库查)给唯一的值,其他列从很小的值域随机取
// (值域小 → 并列、命中、不命中自然出现)。违反约束的行被数据库拒绝,跳过。
// 然后从数据库读回实际的数据交给参考求值器 —— 约束不需要工厂去推断。
//
// 只测查询流水线: get_by_id(单条)、list_keyset(分页,逐页翻完)。计算模式的输出是 AI 的逻辑,由计算例子验证。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/big"
	"math/rand"
	"sort"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/protoload"
	"apifactory/factory/spec"
)

const (
	diffRows     = 40
	diffRequests = 24
	diffPageSize = 3
)

var diffBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type diffEnv struct {
	ctx    context.Context
	conn   *pgx.Conn
	plan   *ir.Plan
	api    *ir.API
	table  string
	cols   []core.NamedType
	rnd    *rand.Rand
	rows   []core.Row
	filter map[string]ir.Filter // 参数名 → 筛选
	input  protoreflect.MessageDescriptor
}

// invalid: 请求按 proto 的校验规则(protovalidate,和服务器同一套规则)是否不合法。
// 随机请求会违反输入之间的规则(例如 created_after < created_before),这时期待 INVALID_ARGUMENT。
func (e *diffEnv) invalid(req map[string]any) (bool, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	msg := dynamicpb.NewMessage(e.input)
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

func runDiff(ctx context.Context, conn *pgx.Conn, url string, plan *ir.Plan, api *ir.API) []Result {
	p := api.Pipeline
	if p == nil || (api.Pattern != "get_by_id" && api.Pattern != "list_keyset") {
		return nil
	}
	fail := func(format string, args ...any) []Result {
		return []Result{{Spec: api.ID, Suite: "diff", Case: "准备", Passed: false, Detail: fmt.Sprintf(format, args...)}}
	}
	h := fnv.New64a()
	h.Write([]byte(api.ID))
	env := &diffEnv{ctx: ctx, conn: conn, plan: plan, api: api, table: p.Steps[0].Table, cols: p.Steps[0].Columns,
		rnd: rand.New(rand.NewSource(int64(h.Sum64()))), filter: map[string]ir.Filter{}}
	for _, f := range api.Filters {
		env.filter[f.Field] = f
	}
	files, err := protoload.Load()
	if err != nil {
		return fail("proto: %v", err)
	}
	md, err := protoload.FindMethod(files, api.ProtoPackage, api.Service, api.Method)
	if err != nil {
		return fail("proto: %v", err)
	}
	env.input = md.Input()
	if _, err := conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		return fail("%v", err)
	}
	inserted, rejected, err := env.load()
	if err != nil {
		return fail("数据: %v", err)
	}
	if inserted < 2 {
		return fail("数据: 只插入了 %d 行(拒绝 %d 行)", inserted, rejected)
	}

	var results []Result
	hits := map[string]int{} // 覆盖: 0 行 / 1 行 / 多行
	for i, params := range env.requests() {
		name := fmt.Sprintf("#%02d %s", i+1, env.describe(params))
		ok, detail, n := env.runOne(url, params)
		switch {
		case n < 0:
			hits["invalid"]++
		case n == 0:
			hits["0"]++
		case n == 1:
			hits["1"]++
		default:
			hits["n"]++
		}
		results = append(results, Result{Spec: api.ID, Suite: "diff", Case: name, Passed: ok, Detail: detail})
	}
	// 覆盖条件: 单条 → 找到和找不到都出现;分页 → 出现多行(要翻页)和 0 行
	cover := fmt.Sprintf("数据 %d 行(拒绝 %d)/ 结果 0 行 %d 次、1 行 %d 次、多行 %d 次、不合法 %d 次", inserted, rejected, hits["0"], hits["1"], hits["n"], hits["invalid"])
	okCover := hits["0"] > 0 && hits["1"]+hits["n"] > 0
	if api.Pattern == "list_keyset" {
		okCover = okCover && hits["n"] > 0
	}
	results = append(results, Result{Spec: api.ID, Suite: "diff", Case: "覆盖条件", Passed: okCover, Detail: cover})
	return results
}

// ---------------------------------------------------------------------------
// 数据
// ---------------------------------------------------------------------------

var textDomain = []string{"a", "b", "B", "a%", "a_", "ab", "abc", "é"}
var intDomain = []int64{1, 2, 3, 10}

func (e *diffEnv) uniqueColumns() (map[string]bool, error) {
	rows, err := e.conn.Query(e.ctx, `
		SELECT kcu.column_name, count(*) OVER (PARTITION BY tc.constraint_name)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
		WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')`, e.table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var col string
		var n int64
		if err := rows.Scan(&col, &n); err != nil {
			return nil, err
		}
		if n == 1 { // 单列的主键 / 唯一约束
			out[col] = true
		}
	}
	return out, rows.Err()
}

func (e *diffEnv) value(c core.NamedType, i int, unique bool) any {
	if c.Type.Nullable && e.rnd.Intn(5) == 0 {
		return nil
	}
	if en := e.plan.Enum(e.table, c.Name); en != nil {
		v := en.Values[e.rnd.Intn(len(en.Values))]
		return v.DB
	}
	switch c.Type.Kind {
	case core.Int32, core.Int64:
		if unique {
			return int64(i + 1)
		}
		return intDomain[e.rnd.Intn(len(intDomain))]
	case core.Text:
		v := textDomain[e.rnd.Intn(len(textDomain))]
		if unique {
			v = fmt.Sprintf("%s-%02d", v, i)
		}
		return v
	case core.Bool:
		return e.rnd.Intn(2) == 0
	case core.Timestamp:
		return diffBase.Add(time.Duration(e.rnd.Intn(4)-1) * time.Hour)
	case core.Date:
		return diffBase.AddDate(0, 0, e.rnd.Intn(3)-1)
	case core.Decimal:
		return []*big.Rat{big.NewRat(3, 2), big.NewRat(9, 1), big.NewRat(10, 1)}[e.rnd.Intn(3)]
	}
	return nil
}

func sqlValue(v any) any {
	switch x := v.(type) {
	case *big.Rat:
		return core.RatString(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	}
	return v
}

// load: 生成 → 逐行插入(违反约束的行被拒绝)→ 读回实际的数据。
func (e *diffEnv) load() (int, int, error) {
	unique, err := e.uniqueColumns()
	if err != nil {
		return 0, 0, err
	}
	ident := pgx.Identifier{e.table}.Sanitize()
	if _, err := e.conn.Exec(e.ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
		return 0, 0, err
	}
	var names, holders []string
	for i, c := range e.cols {
		names = append(names, pgx.Identifier{c.Name}.Sanitize())
		holders = append(holders, fmt.Sprintf("$%d::%s", i+1, c.Type.Kind.SQLType()))
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(names, ", "), strings.Join(holders, ", "))
	rejected := 0
	for i := 0; i < diffRows; i++ {
		var args []any
		for _, c := range e.cols {
			args = append(args, sqlValue(e.value(c, i, unique[c.Name])))
		}
		if _, err := e.conn.Exec(e.ctx, insert, args...); err != nil {
			rejected++
		}
	}
	var sel []string
	for _, c := range e.cols {
		sel = append(sel, pgx.Identifier{c.Name}.Sanitize()+"::text")
	}
	rows, err := e.conn.Query(e.ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(sel, ", "), ident))
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]*string, len(e.cols))
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return 0, 0, err
		}
		r := core.Row{}
		for i, c := range e.cols {
			v, err := core.FromPGText(vals[i], c.Type.Kind)
			if err != nil {
				return 0, 0, err
			}
			r[c.Name] = v
		}
		e.rows = append(e.rows, r)
	}
	return len(e.rows), rejected, rows.Err()
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

func (e *diffEnv) userParams() []core.ParamDecl {
	var out []core.ParamDecl
	for _, d := range e.api.Pipeline.Params {
		if _, ok := e.filter[d.Name]; ok {
			out = append(out, d)
		}
	}
	return out
}

func missing(k core.Kind) any {
	switch k {
	case core.Int32, core.Int64:
		return int64(999)
	case core.Text:
		return "zz-missing"
	case core.Timestamp:
		return diffBase.AddDate(5, 0, 0)
	case core.Date:
		return diffBase.AddDate(5, 0, 0)
	case core.Decimal:
		return big.NewRat(999, 1)
	case core.Bool:
		return true
	}
	return nil
}

// requests: 每个请求以一行数据为"锚"(命中),每个参数随机取: 锚的值 / 另一行的值 / 不存在的值 / 不传(可选时)。
func (e *diffEnv) requests() []core.Params {
	var out []core.Params
	seen := map[string]bool{}
	for i := 0; len(out) < diffRequests && i < diffRequests*4; i++ {
		anchor := e.rows[e.rnd.Intn(len(e.rows))]
		other := e.rows[e.rnd.Intn(len(e.rows))]
		ps := core.Params{}
		for _, d := range e.userParams() {
			f := e.filter[d.Name]
			choice := e.rnd.Intn(4)
			if i == 0 { // 第一个: 可选参数全部不传
				choice = 3
			}
			if choice == 3 && d.Required {
				choice = 0
			}
			var v any
			switch choice {
			case 0:
				v = anchor[f.Column]
			case 1:
				v = other[f.Column]
			case 2:
				if e.plan.Enum(e.table, f.Column) != nil {
					v = other[f.Column] // 枚举: 只用定义过的值(未定义的值被请求校验拒绝)
				} else {
					v = missing(d.Type.Kind)
				}
			case 3:
				continue
			}
			if v == nil {
				if d.Required {
					v = missing(d.Type.Kind)
				} else {
					continue
				}
			}
			if f.Op == "prefix" {
				s := []rune(v.(string))
				v = string(s[:e.rnd.Intn(len(s)+1)])
				if v == "" && !d.Required {
					continue
				}
			}
			ps[d.Name] = v
		}
		key := e.describe(ps)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ps)
	}
	return out
}

func (e *diffEnv) describe(ps core.Params) string {
	var keys []string
	for k := range ps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+core.Canonical(ps[k]))
	}
	if len(parts) == 0 {
		return "(不传可选条件)"
	}
	return strings.Join(parts, " ")
}

// present: 核心的值 → 请求 / 期待值里的表示(与 protojson 一致)。column 用来查枚举。
func (e *diffEnv) present(column string, v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		if t, _ := e.colType(column); t.Kind == core.Date {
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case *big.Rat:
		return core.RatString(x)
	case int64:
		if en := e.plan.Enum(e.table, column); en != nil {
			for _, ev := range en.Values {
				if ev.DB == x {
					return ev.Name
				}
			}
			return en.Unspecified
		}
	}
	return v
}

func (e *diffEnv) colType(name string) (core.Type, bool) {
	return core.Schema(e.cols).Col(name)
}

func (e *diffEnv) request(ps core.Params) map[string]any {
	req := map[string]any{}
	for k, v := range ps {
		req[k] = e.present(e.filter[k].Column, v)
	}
	return req
}

func (e *diffEnv) item(r core.Row, msg *ir.Message) map[string]any {
	out := map[string]any{}
	for _, f := range msg.Fields {
		v := e.present(f.Name, r[f.Name])
		if v == nil {
			if f.Presence {
				out[f.Name] = "$absent"
			}
			continue
		}
		out[f.Name] = v
	}
	return out
}

// runOne: 一个请求。返回 (通过, 说明, 参考求值器的结果行数)。
func (e *diffEnv) runOne(url string, ps core.Params) (bool, string, int) {
	evalParams := core.Params{}
	for k, v := range ps {
		evalParams[k] = v
	}
	if e.api.Pattern == "list_keyset" {
		evalParams["row_limit"] = int64(1 << 30) // 完整的有序结果,再按页切
	}
	res, err := core.Eval(e.api.Pipeline, map[string][]core.Row{e.table: e.rows}, evalParams)
	if errors.Is(err, core.ErrAmbiguous) {
		return false, "参考求值器: " + err.Error() + "(排序 / 键不唯一,设计的问题)", 0
	}
	if err != nil {
		return false, "参考求值器: " + err.Error(), 0
	}
	n := len(res.Rows)
	req := e.request(ps)
	probe := map[string]any{}
	for k, v := range req {
		probe[k] = v
	}
	if e.api.Pattern == "list_keyset" {
		probe["page_size"] = diffPageSize
	}
	bad, err := e.invalid(probe)
	if err != nil {
		return false, "请求校验: " + err.Error(), n
	}
	if bad { // 不合法的请求: 期待 INVALID_ARGUMENT(不计入覆盖)
		ok, detail := runCase(url, spec.Case{Request: probe, Expect: spec.Expect{Code: "INVALID_ARGUMENT"}}, &e.api.Response)
		return ok, "不合法的请求: " + detail, -1
	}
	switch e.api.Pattern {
	case "get_by_id":
		var c spec.Case
		switch {
		case n == 0:
			c = spec.Case{Request: req, Expect: spec.Expect{Code: "NOT_FOUND"}}
		case n == 1:
			c = spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: e.item(res.Rows[0], &e.api.Response)}}
		default:
			return false, fmt.Sprintf("单条: 按键筛选得到 %d 行(键不唯一,设计规则的问题)", n), n
		}
		ok, detail := runCase(url, c, &e.api.Response)
		return ok, detail, n
	case "list_keyset":
		items := e.api.Keyset.ItemsField
		msg := e.api.Response.Field(items).Message
		var steps []spec.Step
		for start := 0; start == 0 || start < n; start += diffPageSize {
			end := min(start+diffPageSize, n)
			list := []any{}
			for _, r := range res.Rows[start:end] {
				list = append(list, e.item(r, msg))
			}
			stepReq := map[string]any{"page_size": diffPageSize}
			for k, v := range req {
				stepReq[k] = v
			}
			if start > 0 {
				stepReq["page_token"] = "$prev.next_page_token"
			}
			token := any("$absent")
			if end < n {
				token = "$nonempty"
			}
			steps = append(steps, spec.Step{Request: stepReq, Expect: spec.Expect{Code: "OK", Response: map[string]any{items: list, "next_page_token": token}}})
		}
		ok, detail := runCase(url, spec.Case{Steps: steps}, &e.api.Response)
		return ok, fmt.Sprintf("%d 行 / %d 页: %s", n, len(steps), detail), n
	}
	return false, "不支持的模式", n
}
