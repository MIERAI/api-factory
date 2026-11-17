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
	"apifactory/factory/kinds"
	"apifactory/factory/protoload"
	"apifactory/factory/spec"
)

const (
	diffRows     = 40
	diffRequests = 40
	diffPageSize = 3
)

type diffEnv struct {
	ctx    context.Context
	conn   *pgx.Conn // 主库
	db     *dbs      // 主库 + 分库
	plan   *ir.Plan
	api    *ir.API
	table  string
	cols   []core.NamedType
	rnd    *rand.Rand
	rows   []core.Row
	filter map[string]ir.Filter // 参数名 → 筛选
	input  protoreflect.MessageDescriptor
	json   map[string][]jsonKey // JSON 列 → 流水线里读到的键(json_field)
	out    core.Schema          // 流水线最后的列(响应的值按这里的类型表示)
	// anchors: 请求参数取值的来源 = 来源部分(scan / 来源单元)的输出。
	// 普通的表 = 物理行;JSON版本 = 展开后的逻辑行(筛选的列在物理表里不存在)
	anchors []core.Row
	consts  map[core.Kind][]any    // 流水线里出现的常量(加进值域: 删除值、逻辑删除值等要能被随机数据碰到)
	ranges  map[string][]int64     // 列 → 整数值域(筛选这一列的参数有范围校验时,从范围里取,否则请求都不合法)
	formats map[string]*kinds.Type // 列 → 带格式的类型(参数有格式校验时,数据要符合格式)
	keyCol  map[string]string      // JSON 的键 → 逻辑列名(json_field 投影成的列)
}

type jsonKey struct {
	key  string
	kind core.Kind
}

// collect: 遍历流水线的表达式,收集 json_field 的键和常量。
func (e *diffEnv) collect() {
	e.json, e.consts = map[string][]jsonKey{}, map[core.Kind][]any{}
	seen := map[string]bool{}
	var walk func(x *core.Expr)
	e.keyCol = map[string]string{}
	walk = func(x *core.Expr) {
		if x == nil {
			return
		}
		switch x.Op {
		case "json_field":
			if c := x.Args[0]; c.Op == "col" && !seen[c.Name+"."+x.Name+string(x.Type.Kind)] {
				seen[c.Name+"."+x.Name+string(x.Type.Kind)] = true
				e.json[c.Name] = append(e.json[c.Name], jsonKey{x.Name, x.Type.Kind})
			}
		case "const":
			if x.Value != nil && x.Type != nil {
				e.consts[x.Type.Kind] = append(e.consts[x.Type.Kind], x.Value)
			}
		}
		for _, a := range x.Args {
			walk(a)
		}
	}
	for _, st := range e.api.Pipeline.Steps {
		walk(st.Pred)
		for _, x := range st.Project {
			walk(x.Expr)
			if x.Expr.Op == "json_field" {
				e.keyCol[x.Expr.Name] = x.Name
			}
		}
	}
	// 整数参数的范围校验 → 被它筛选的列(多列单元的每一列)的值域
	e.ranges = map[string][]int64{}
	e.formats = map[string]*kinds.Type{}
	for _, f := range e.api.Filters {
		fd := e.api.Request.Field(f.Field)
		if fd == nil || fd.Rules == nil {
			continue
		}
		r := fd.Rules
		if t, ok := kinds.ByPattern(r.Pattern); ok && len(t.Domain) > 0 { // 带格式的文本(邮编 …): 值域用这个类型的
			for _, c := range append([]string{f.Column}, f.Columns...) {
				e.formats[c] = t
			}
			continue
		}
		var lo int64
		switch {
		case r.Gte != nil:
			lo = *r.Gte
		case r.Gt != nil:
			lo = *r.Gt + 1
		default:
			continue
		}
		hi := lo + 10
		if r.Lte != nil {
			hi = *r.Lte
		} else if r.Lt != nil {
			hi = *r.Lt - 1
		}
		dom := []int64{lo, lo + 1, lo + 2, hi}
		cols := f.Columns
		if len(cols) == 0 {
			cols = []string{f.Column}
		}
		for _, c := range cols {
			e.ranges[c] = dom
		}
	}
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

func runDiff(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API) []Result {
	conn := d.main
	p := api.Pipeline
	if p == nil || (api.Pattern != "get_by_id" && api.Pattern != "list_keyset" && api.Pattern != "list_all") {
		return nil
	}
	fail := func(format string, args ...any) []Result {
		return []Result{{Spec: api.ID, Suite: "diff", Case: "准备", Passed: false, Detail: fmt.Sprintf(format, args...)}}
	}
	h := fnv.New64a()
	h.Write([]byte(api.ID))
	env := &diffEnv{ctx: ctx, conn: conn, db: d, plan: plan, api: api, table: p.Steps[0].Table, cols: p.Steps[0].Columns,
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
	env.collect()
	if env.out, err = core.Check(p); err != nil {
		return fail("流水线: %v", err)
	}
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
	if err := env.loadAnchors(); err != nil {
		return fail("来源: %v", err)
	}

	var results []Result
	hits := map[string]int{} // 覆盖: 0 行 / 1 行 / 多行
	for i, params := range env.requests() {
		name := fmt.Sprintf("#%02d %s", i+1, env.describe(params))
		ok, detail, n := env.runOne(url, params)
		if api.ListAll != nil && n > int(api.ListAll.MaxRows) {
			hits["over"]++
		}
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
	if api.Pattern == "list_keyset" {
		results = append(results, env.pageExitCases(url)...)
	}
	// 覆盖条件: 单条 → 找到和找不到都出现;分页 → 出现多行(要翻页)和 0 行
	cover := fmt.Sprintf("数据 %d 行(拒绝 %d,来源 %d 行)/ 结果 0 行 %d 次、1 行 %d 次、多行 %d 次、不合法 %d 次", inserted, rejected, len(env.anchors), hits["0"], hits["1"], hits["n"], hits["invalid"])
	okCover := hits["0"] > 0 && hits["1"]+hits["n"] > 0
	if len(env.userParams()) == 0 { // 没有输入: 只有一种请求,"0 行"的情况不存在
		okCover = hits["1"]+hits["n"] > 0
	}
	if api.Pattern == "list_keyset" || api.Pattern == "list_all" {
		okCover = okCover && hits["n"] > 0
	}
	if api.ListAll != nil {
		cover += fmt.Sprintf("、超过上限 %d 次", hits["over"])
		if hits["over"] == 0 {
			cover += fmt.Sprintf("(上限 %d ≥ 测试数据量,超限的出口行为没有测到)", api.ListAll.MaxRows)
		}
	}
	results = append(results, Result{Spec: api.ID, Suite: "diff", Case: "覆盖条件", Passed: okCover, Detail: cover})
	return results
}

// ---------------------------------------------------------------------------
// 数据
// ---------------------------------------------------------------------------

// uniqueColumns: 需要唯一序号的列。单列的主键 / 唯一约束 → 这一列;
// 复合的 → 最后一列(前面的列保持小值域,形成分组: 例如 (order_id, line_no) 每个订单有多行)。
func (e *diffEnv) uniqueColumns() (map[string]bool, error) {
	rows, err := e.conn.Query(e.ctx, `
		SELECT kcu.column_name,
		       kcu.ordinal_position = max(kcu.ordinal_position) OVER (PARTITION BY tc.constraint_name)
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
		var last bool
		if err := rows.Scan(&col, &last); err != nil {
			return nil, err
		}
		if last {
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
	if c.Type.Kind == core.JSON { // 按流水线读到的键生成对象;有时缺少某个键
		obj := map[string]any{}
		for _, k := range e.json[c.Name] {
			if e.rnd.Intn(6) == 0 {
				continue
			}
			v := e.scalarFor(e.keyCol[k.key], k.kind, false, i)
			switch x := v.(type) {
			case *big.Rat:
				v = core.RatString(x)
			case time.Time:
				if k.kind == core.Date {
					v = x.Format("2006-01-02")
				} else {
					v = x.Format(time.RFC3339)
				}
			}
			obj[k.key] = v
		}
		b, _ := json.Marshal(obj)
		return string(b)
	}
	return e.scalarFor(c.Name, c.Type.Kind, unique, i)
}

// scalarFor: 某一列的值(这一列有范围值域时从里面取)。
func (e *diffEnv) scalarFor(col string, kind core.Kind, unique bool, i int) any {
	if t, ok := e.formats[col]; ok && !unique {
		return t.Domain[e.rnd.Intn(len(t.Domain))]
	}
	if dom, ok := e.ranges[col]; ok && !unique && (kind == core.Int32 || kind == core.Int64) {
		return dom[e.rnd.Intn(len(dom))]
	}
	return e.scalar(kind, unique, i)
}

func (e *diffEnv) scalar(kind core.Kind, unique bool, i int) any {
	if cs := e.consts[kind]; len(cs) > 0 && !unique && e.rnd.Intn(8) == 0 {
		return cs[e.rnd.Intn(len(cs))] // 流水线里的常量
	}
	t, ok := kinds.ByCore(kind) // 值域来自类型的注册表(factory/kinds)
	if !ok || len(t.Domain) == 0 {
		return nil
	}
	if _, isInt := t.Domain[0].(int64); isInt && unique { // 唯一列: 序号
		return int64(i + 1)
	}
	v := t.Domain[e.rnd.Intn(len(t.Domain))]
	if s, isText := v.(string); isText && unique {
		return fmt.Sprintf("%s-%02d", s, i)
	}
	return v
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
	sh := e.plan.Shard(e.api.Table)
	targets, err := e.db.targets(sh)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range targets {
		if _, err := c.Exec(e.ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
			return 0, 0, err
		}
	}
	// 分库键在物理行里的位置: 普通的表 = 列;JSON版本 = JSON 里的键
	shardCol, shardKey := "", ""
	if sh != nil {
		shardCol = sh.Column
		if st := e.plan.Storage(e.api.Table); st != nil {
			shardCol = st.DataColumn
			for _, f := range st.Fields {
				if f.Name == sh.Column {
					shardKey = f.Key
				}
			}
		}
	}
	var names, holders []string
	for i, c := range e.cols {
		names = append(names, pgx.Identifier{c.Name}.Sanitize())
		holders = append(holders, fmt.Sprintf("$%d::%s", i+1, c.Type.Kind.SQLType()))
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(names, ", "), strings.Join(holders, ", "))
	rejected := 0
	n := diffRows
	if len(e.json) > 0 { // 多版本的来源(JSON版本 等): 一个键有好几行,多生成一些
		n = diffRows * 3
	}
	for i := 0; i < n; i++ {
		var args []any
		conn := e.conn
		for _, c := range e.cols {
			v := e.value(c, i, unique[c.Name])
			if sh != nil && c.Name == shardCol { // 分库: 写进分库键对应的库
				key := v
				if shardKey != "" {
					var obj map[string]any
					if s, ok := v.(string); ok && json.Unmarshal([]byte(s), &obj) == nil {
						key = obj[shardKey]
					}
				}
				if conn, err = e.db.forKey(key); err != nil {
					return 0, 0, err
				}
			}
			args = append(args, sqlValue(v))
		}
		if _, err := conn.Exec(e.ctx, insert, args...); err != nil {
			rejected++
		}
	}
	var sel []string
	for _, c := range e.cols {
		sel = append(sel, pgx.Identifier{c.Name}.Sanitize()+"::text")
	}
	for _, conn := range targets { // 读回: 所有的库合在一起
		rows, err := conn.Query(e.ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(sel, ", "), ident))
		if err != nil {
			return 0, 0, err
		}
		for rows.Next() {
			vals := make([]*string, len(e.cols))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return 0, 0, err
			}
			r := core.Row{}
			for i, c := range e.cols {
				v, err := core.FromPGText(vals[i], c.Type.Kind)
				if err != nil {
					rows.Close()
					return 0, 0, err
				}
				r[c.Name] = v
			}
			e.rows = append(e.rows, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, 0, err
		}
	}
	return len(e.rows), rejected, nil
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
	if t, ok := kinds.ByCore(k); ok {
		return t.Missing
	}
	return nil
}

// requests: 每个请求以一行数据为"锚"(命中),每个参数随机取: 锚的值 / 另一行的值 / 不存在的值 / 不传(可选时)。
func (e *diffEnv) requests() []core.Params {
	var out []core.Params
	seen := map[string]bool{}
	for i := 0; len(out) < diffRequests && i < diffRequests*4; i++ {
		anchor := e.anchors[e.rnd.Intn(len(e.anchors))]
		other := e.anchors[e.rnd.Intn(len(e.anchors))]
		ps := core.Params{}
		for _, d := range e.userParams() {
			f := e.filter[d.Name]
			if d.List { // 列表参数: 0~3 个值(锚的值 / 别的行的值 / 不存在的值);必填至少 1 个
				n := e.rnd.Intn(4)
				if i == 0 && !d.Required {
					continue
				}
				if d.Required && n == 0 {
					n = 1
				}
				l := []any{}
				for j := 0; j < n; j++ {
					var v any
					switch {
					case j == 0 && e.rnd.Intn(4) > 0: // 1/4 的机会不放锚的值(可能 0 行)
						v = anchor[f.Column]
					case j == 0:
						v = e.missingFor(f.Column, d.Type.Kind)
					case e.rnd.Intn(3) == 0:
						v = e.missingFor(f.Column, d.Type.Kind)
					default:
						v = e.anchors[e.rnd.Intn(len(e.anchors))][f.Column]
					}
					if v != nil {
						l = append(l, v)
					}
				}
				if d.Required && len(l) == 0 {
					l = append(l, e.missingFor(f.Column, d.Type.Kind))
				}
				ps[d.Name] = l
				continue
			}
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
				if e.plan.Enum(e.api.Table, f.Column) != nil {
					v = other[f.Column] // 枚举: 只用定义过的值(未定义的值被请求校验拒绝)
				} else {
					v = e.missingFor(f.Column, d.Type.Kind)
				}
			case 3:
				continue
			}
			if v == nil {
				if d.Required {
					v = e.missingFor(f.Column, d.Type.Kind)
				} else {
					continue
				}
			}
			if f.Op == "prefix" {
				s := []rune(v.(string))
				cut := e.rnd.Intn(len(s) + 1)
				// 陷阱前缀: 有一半的机会截在 % 或 _ 之后(它们必须按字面匹配,不是通配符)
				for j, r := range s {
					if (r == '%' || r == '_') && e.rnd.Intn(2) == 0 {
						cut = j + 1
						break
					}
				}
				v = string(s[:cut])
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
	case []any:
		out := make([]any, len(x))
		for i, y := range x {
			out[i] = e.present(column, y)
		}
		return out
	case time.Time:
		if t, _ := e.colType(column); t.Kind == core.Date {
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case *big.Rat:
		return core.RatString(x)
	case int64:
		if en := e.plan.Enum(e.api.Table, column); en != nil {
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

// colType: 输出 / 筛选的列的类型(先看流水线的输出,再看扫描的表)
func (e *diffEnv) colType(name string) (core.Type, bool) {
	if t, ok := e.out.Col(name); ok {
		return t, true
	}
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
	case "list_all":
		la := e.api.ListAll
		if n > int(la.MaxRows) { // 多取的那一行: 超过上限 → 报错,不截断
			ok, detail := runCase(url, spec.Case{Request: req, Expect: spec.Expect{Code: "FAILED_PRECONDITION", MessageContains: "too many"}}, &e.api.Response)
			return ok, fmt.Sprintf("超过上限(%d 行以上): %s", n, detail), n
		}
		msg := e.api.Response.Field(la.ItemsField).Message
		list := []any{}
		for _, r := range res.Rows {
			list = append(list, e.item(r, msg))
		}
		ok, detail := runCase(url, spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: map[string]any{la.ItemsField: list}}}, &e.api.Response)
		return ok, fmt.Sprintf("%d 行: %s", n, detail), n
	case "list_keyset":
		items := e.api.Keyset.ItemsField
		msg := e.api.Response.Field(items).Message
		var steps []spec.Step
		size := []int{1, diffPageSize, diffPageSize}[e.rnd.Intn(3)] // 1: 并列行跨页
		omitSize := e.rnd.Intn(4) == 0                              // 有时不传 page_size: 用式样书的默认值
		if omitSize {
			size = int(e.api.Keyset.DefaultSize)
		}
		for start := 0; start == 0 || start < n; start += size {
			end := min(start+size, n)
			list := []any{}
			for _, r := range res.Rows[start:end] {
				list = append(list, e.item(r, msg))
			}
			stepReq := map[string]any{"page_size": size}
			for k, v := range req {
				stepReq[k] = v
			}
			if omitSize {
				delete(stepReq, "page_size")
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

// pageExitCases: 分页出口自己的行为(不在 SQL 流水线里): 途中改 page_size / 途中改筛选条件 / 非法 token。
// 期待值同样来自参考求值器。
func (e *diffEnv) pageExitCases(url string) []Result {
	items := e.api.Keyset.ItemsField
	msg := e.api.Response.Field(items).Message
	res := func(name string, c spec.Case) Result {
		ok, detail := runCase(url, c, &e.api.Response)
		return Result{Spec: e.api.ID, Suite: "diff", Case: "分页出口: " + name, Passed: ok, Detail: detail}
	}
	// 找一个合法、结果 ≥ 5 行的请求做基础
	var base core.Params
	var rows []core.Row
	for _, ps := range e.requests() {
		req := e.request(ps)
		req["page_size"] = 1
		if bad, err := e.invalid(req); err != nil || bad {
			continue
		}
		evalParams := core.Params{"row_limit": int64(1 << 30)}
		for k, v := range ps {
			evalParams[k] = v
		}
		r, err := core.Eval(e.api.Pipeline, map[string][]core.Row{e.table: e.rows}, evalParams)
		if err == nil && len(r.Rows) >= 5 {
			base, rows = ps, r.Rows
			break
		}
	}
	if base == nil {
		return []Result{{Spec: e.api.ID, Suite: "diff", Case: "分页出口", Passed: false, Detail: "找不到结果 ≥ 5 行的请求(覆盖不足)"}}
	}
	list := func(rs []core.Row) []any {
		out := []any{}
		for _, r := range rs {
			out = append(out, e.item(r, msg))
		}
		return out
	}
	with := func(extra map[string]any) map[string]any {
		req := e.request(base)
		for k, v := range extra {
			req[k] = v
		}
		return req
	}
	out := []Result{
		res("途中改 page_size 是允许的", spec.Case{Steps: []spec.Step{
			{Request: with(map[string]any{"page_size": 2}), Expect: spec.Expect{Code: "OK", Response: map[string]any{items: list(rows[:2]), "next_page_token": "$nonempty"}}},
			{Request: with(map[string]any{"page_size": 3, "page_token": "$prev.next_page_token"}), Expect: spec.Expect{Code: "OK", Response: map[string]any{items: list(rows[2:5])}}},
		}}),
		res("非法 page_token → INVALID_ARGUMENT", spec.Case{Request: with(map[string]any{"page_token": "not-a-valid-token"}),
			Expect: spec.Expect{Code: "INVALID_ARGUMENT", MessageContains: "page_token"}}),
	}
	// 途中改筛选条件 → 旧 token 失效: 改一个筛选参数的值(或者加上一个),改了之后的请求本身必须合法
	for _, d := range e.userParams() {
		f := e.filter[d.Name]
		for _, r := range e.rows {
			v := r[f.Column]
			if v == nil || core.Canonical(v) == core.Canonical(base[d.Name]) {
				continue
			}
			nv := e.present(f.Column, v)
			if d.List {
				nv = []any{nv}
			}
			changed := with(map[string]any{d.Name: nv, "page_size": 1, "page_token": "$prev.next_page_token"})
			probe := with(map[string]any{d.Name: nv, "page_size": 1})
			if bad, err := e.invalid(probe); err != nil || bad {
				continue
			}
			out = append(out, res(fmt.Sprintf("途中改筛选条件(%s)→ INVALID_ARGUMENT", d.Name), spec.Case{Steps: []spec.Step{
				{Request: with(map[string]any{"page_size": 1}), Expect: spec.Expect{Code: "OK", Response: map[string]any{"next_page_token": "$nonempty"}}},
				{Request: changed, Expect: spec.Expect{Code: "INVALID_ARGUMENT", MessageContains: "page_token"}},
			}}))
			return out
		}
	}
	return append(out, Result{Spec: e.api.ID, Suite: "diff", Case: "分页出口: 途中改筛选条件", Passed: len(e.userParams()) == 0,
		Detail: "没有可以改的筛选参数"})
}

// loadAnchors: 只跑流水线的来源部分(编译器标记为 datasource.table / 来源 … 的步骤)。
func (e *diffEnv) loadAnchors() error {
	src := &core.Pipeline{Params: e.api.Pipeline.Params}
	for _, st := range e.api.Pipeline.Steps {
		if st.From != "datasource.table" && !strings.HasPrefix(st.From, "来源 ") {
			break
		}
		src.Steps = append(src.Steps, st)
	}
	if len(src.Steps) <= 1 {
		e.anchors = e.rows
		return nil
	}
	res, err := core.Eval(src, map[string][]core.Row{e.table: e.rows}, core.Params{})
	if err != nil {
		return err
	}
	e.anchors = res.Rows
	if len(e.anchors) == 0 {
		return fmt.Errorf("来源部分没有行(数据都被删除了?)")
	}
	return nil
}

// missingFor: 数据里不存在的值(带格式的列用这个类型的,否则请求会被格式校验拒绝)。
func (e *diffEnv) missingFor(col string, k core.Kind) any {
	if t, ok := e.formats[col]; ok {
		return t.Missing
	}
	return missing(k)
}
