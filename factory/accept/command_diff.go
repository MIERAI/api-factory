package accept

// 命令的状态差分测试(设计稿 §5.2): 随机的初始状态 + 随机的请求序列。
// 每个请求之后比较: 响应(含错误码)+ 写的表的完整状态,一边是被测服务器 + 数据库,一边是模型状态
// (factory/command: 内存里的行,按设计稿的语义执行,不看 SQL 和生成的代码)。
//
// 请求序列按"意图"排: 键取现有的行 / 不存在的键(包括被删掉的行),版本号取对的 / 错的,新建的键取新的 / 已有的;
// 每种意图有最低的个数,保证每种结果(成功、不存在、版本冲突、唯一约束冲突)都出现 ≥ minOutcome 次 ——
// 变异(去掉版本检查、版本不加 1、错误码换掉 …)每个至少被 3 个请求抓到。
// 不一致时把模型同步成数据库的状态再继续(一个 bug 不连带后面所有的请求)。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/big"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/command"
	"apifactory/factory/constraints"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
	"apifactory/factory/protoload"
)

const (
	cmdInitialRows = 12 // 初始状态的行数
	cmdRequests    = 48 // 请求序列的长度
	minOutcome     = 4  // 每种结果至少出现的次数(变异的余量要求 ≥ 3)
)

type cmdEnv struct {
	ctx    context.Context
	conn   *pgx.Conn
	plan   *ir.Plan
	api    *ir.API
	cmd    *ir.Command
	t      *command.Table
	rnd    *rand.Rand
	val    validator
	state  *command.State
	byCol  map[string]*ir.Field // 列 → 请求字段(写入 / 键 / 乐观锁)
	pools  map[string][]any     // 列 → 合法的值(数据库的写法: int64 / string / time.Time / bool)
	keys   [][]any              // 主键的候选(初始状态取一部分,剩下的是"不存在的键")
	base   map[string]any       // 合法的基础请求(人写的第一个成功的用例)
	t0     time.Time
	locked bool
	rows0  int // 初始状态的行数
}

func runCommandDiff(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API) []Result {
	fail := func(format string, args ...any) []Result {
		return []Result{{Spec: api.ID, Suite: "diff", Case: "准备", Passed: false, Detail: fmt.Sprintf(format, args...)}}
	}
	conn := d.main
	h := fnv.New64a()
	h.Write([]byte(api.ID + os.Getenv("FACTORY_DIFF_SEED"))) // 换种子: 检查覆盖不是靠运气
	e := &cmdEnv{ctx: ctx, conn: conn, plan: plan, api: api, cmd: api.Command, rnd: rand.New(rand.NewSource(int64(h.Sum64()))),
		byCol: map[string]*ir.Field{}, pools: map[string][]any{}, base: api.BaseRequest, t0: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	files, err := protoload.Load()
	if err != nil {
		return fail("proto: %v", err)
	}
	md, err := protoload.FindMethod(files, api.ProtoPackage, api.Service, api.Method)
	if err != nil {
		return fail("proto: %v", err)
	}
	e.val = validator{md: md.Input()}
	if _, err := conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		return fail("%v", err)
	}
	if e.t, err = command.LoadTable(ctx, conn, e.cmd.Table); err != nil {
		return fail("%v", err)
	}
	for _, b := range append(append([]ir.CommandCol{}, e.cmd.Key...), e.cmd.Write...) {
		e.byCol[b.Column] = api.Request.Field(b.Field)
	}
	if e.cmd.Lock != nil {
		e.byCol[e.cmd.Lock.Column] = api.Request.Field(e.cmd.Lock.Field)
		e.locked = true
	}
	if err := e.load(); err != nil {
		return fail("初始状态: %v", err)
	}

	intents := e.intents()
	var results []Result
	counts := map[command.Outcome]int{}
	for i, in := range intents {
		r := e.step(url, i, in)
		results = append(results, r.res)
		if r.outcome != "" {
			counts[r.outcome]++
		}
	}
	return append(results, e.coverage(counts))
}

// load: 清空 → 随机的行逐行插入(违反约束的被数据库拒绝,跳过)→ 读回实际的数据 = 模型的初始状态。
func (e *cmdEnv) load() error {
	if _, err := e.conn.Exec(e.ctx, "TRUNCATE "+pgx.Identifier{e.t.Name}.Sanitize()+" CASCADE"); err != nil {
		return err
	}
	for _, c := range e.t.Cols {
		e.pools[c.Name] = e.poolFor(c)
	}
	e.keys = e.keySpace(cmdInitialRows * 2)
	perm := e.rnd.Perm(len(e.keys))
	inserted := 0
	for _, i := range perm[:min(len(perm), cmdInitialRows)] {
		row := map[string]any{}
		for j, k := range e.t.PK {
			row[k] = e.keys[i][j]
		}
		for _, c := range e.t.Cols {
			if _, isKey := row[c.Name]; isKey {
				continue
			}
			if p := e.pools[c.Name]; len(p) > 0 && (c.NotNull || e.rnd.Intn(4) > 0) {
				row[c.Name] = p[e.rnd.Intn(len(p))]
			}
		}
		if e.insertRaw(row) == nil {
			inserted++
		}
	}
	if inserted < 3 {
		return fmt.Errorf("只插入了 %d 行(其他被数据库的约束拒绝)", inserted)
	}
	rows, err := command.ReadRows(e.ctx, e.conn, e.t)
	if err != nil {
		return err
	}
	e.rows0 = len(rows)
	e.state = &command.State{Tables: map[string]*command.TableState{e.t.Name: {T: e.t, Rows: rows}}}
	for _, fk := range append(append([]command.FK{}, e.t.FKs...), e.t.RefBy...) { // 外键相关的表: 现在的数据(不另外造)
		rt, err := command.LoadTable(e.ctx, e.conn, fk.RefTable)
		if err != nil {
			return err
		}
		rr, err := command.ReadRows(e.ctx, e.conn, rt)
		if err != nil {
			return err
		}
		e.state.Tables[rt.Name] = &command.TableState{T: rt, Rows: rr}
	}
	return nil
}

func (e *cmdEnv) insertRaw(row map[string]any) error {
	cols := make([]string, 0, len(row))
	for c := range row {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	names, holders, args := make([]string, len(cols)), make([]string, len(cols)), make([]any, len(cols))
	for i, c := range cols {
		names[i], holders[i], args[i] = pgx.Identifier{c}.Sanitize(), "$"+strconv.Itoa(i+1), row[c]
	}
	_, err := e.conn.Exec(e.ctx, fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", pgx.Identifier{e.t.Name}.Sanitize(),
		strings.Join(names, ","), strings.Join(holders, ",")), args...)
	return err
}

// poolFor: 一列的值域(数据库的写法)。请求字段有约束(格式 / 范围 …)或带格式的类型 → 约束的值域;枚举 → 数据库值;
// 否则按类型取很小的值域(相同的值经常出现)。和请求字段对应的列: 只留下请求合法的值。
func (e *cmdEnv) poolFor(c command.Column) []any {
	var vals []any
	f := e.byCol[c.Name]
	if en := e.plan.Enum(e.t.Name, c.Name); en != nil {
		for _, v := range en.Values {
			vals = append(vals, v.DB)
		}
		return vals
	}
	if d := constraints.DomainOf(f); d != nil {
		vals = append(vals, d.Values...)
		if d.Expand != nil {
			for i := 0; i < 6; i++ {
				vals = append(vals, d.Expand(i))
			}
		}
	} else if t := patternKind(f); t != nil && len(t.Domain) > 0 {
		vals = append(vals, t.Domain...)
	}
	if len(vals) == 0 {
		vals = genericPool(c)
	}
	for i, v := range vals {
		vals[i] = dbForm(c.Type, v)
	}
	if f == nil {
		return vals
	}
	var ok []any
	for _, v := range vals {
		if e.fieldValid(f, c, v) {
			ok = append(ok, v)
		}
	}
	return ok
}

// patternKind: 请求字段是带格式的类型(非负十进制 / 邮编 …)时,这个类型。
func patternKind(f *ir.Field) *kinds.Type {
	if f == nil || f.Rules == nil {
		return nil
	}
	var r struct {
		String struct {
			Pattern string `json:"pattern"`
		} `json:"string"`
	}
	if json.Unmarshal(f.Rules.Raw, &r) != nil || r.String.Pattern == "" {
		return nil
	}
	t, _ := kinds.ByPattern(r.String.Pattern)
	return t
}

func genericPool(c command.Column) []any {
	switch c.Type {
	case "int2", "int4", "int8":
		return []any{int64(1), int64(2), int64(3), int64(5), int64(8)}
	case "numeric":
		return []any{"0", "1.5", "9", "10", "12.50", "1300"}
	case "timestamptz", "timestamp":
		t := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
		return []any{t, t.Add(90 * time.Minute), t.Add(24 * time.Hour)}
	case "date":
		return []any{"2026-10-01", "2026-10-02", "2026-11-30"}
	case "bool":
		return []any{true, false}
	case "float8", "float4":
		return []any{0.5, 1.0, 2.25}
	}
	short := strings.ToLower(c.Name)
	if len(short) > 3 {
		short = short[:3]
	}
	return []any{short + "-a", short + "-b", short + "-c", short + "-dd"}
}

// dbForm: 值域里的值 → 数据库的写法(十进制的 big.Rat → 十进制的文本)。
func dbForm(typ string, v any) any {
	switch x := v.(type) {
	case *big.Rat:
		return ratText(x)
	case int:
		return int64(x)
	case int32:
		return int64(x)
	}
	return v
}

func ratText(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	s := r.FloatString(6)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

// keySpace: 主键的候选(n 个不同的、请求合法的值)。
func (e *cmdEnv) keySpace(n int) [][]any {
	var out [][]any
	seen := map[string]bool{}
	for attempt := 0; len(out) < n && attempt < n*20; attempt++ {
		k := make([]any, len(e.t.PK))
		for j, col := range e.t.PK {
			c := e.t.Col(col)
			k[j] = e.keyValue(*c, attempt, j)
		}
		key := fmt.Sprint(k...)
		if seen[key] {
			continue
		}
		ok := true
		for j, col := range e.t.PK {
			if f := e.byCol[col]; f != nil && !e.fieldValid(f, *e.t.Col(col), k[j]) {
				ok = false
			}
		}
		if ok {
			seen[key] = true
			out = append(out, k)
		}
	}
	return out
}

// keyValue: 主键的第 i 个候选。整数 = 1, 2, …(范围的下限之后);文本 = 值域(+ 扩大)或者 列名-序号。
func (e *cmdEnv) keyValue(c command.Column, i, j int) any {
	switch c.Type {
	case "int2", "int4", "int8":
		return int64(i + 1)
	}
	f := e.byCol[c.Name]
	if d := constraints.DomainOf(f); d != nil {
		if d.Expand != nil {
			return d.Expand(i)
		}
		if i < len(d.Values) {
			return dbForm(c.Type, d.Values[i])
		}
	}
	if p := e.pools[c.Name]; i < len(p) && e.plan.Enum(e.t.Name, c.Name) != nil {
		return p[i]
	}
	return fmt.Sprintf("%s-%03d", strings.ToUpper(c.Name[:min(3, len(c.Name))]), i+1)
}

// fieldValid: 字段 = v(其他字段 = 基础请求)的请求是否合法。
func (e *cmdEnv) fieldValid(f *ir.Field, c command.Column, v any) bool {
	req := map[string]any{}
	for k, x := range e.base {
		req[k] = x
	}
	req[f.Name] = e.jsonValue(f, c, v)
	bad, err := e.val.invalid(req)
	return err == nil && !bad
}

// jsonValue: 数据库的写法 → 请求里的写法(protojson)。
func (e *cmdEnv) jsonValue(f *ir.Field, c command.Column, v any) any {
	if f.Kind == "enum" {
		if en := e.plan.Enum(e.t.Name, c.Name); en != nil {
			for _, x := range en.Values {
				if fmt.Sprint(x.DB) == fmt.Sprint(v) {
					return x.Name
				}
			}
		}
		return v
	}
	switch x := v.(type) {
	case time.Time:
		if f.Kind == "string" { // 日期
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case int64:
		if f.Kind == "int64" || f.Kind == "uint64" {
			return strconv.FormatInt(x, 10)
		}
		return x
	}
	return v
}

// intent: 一个请求的意图。
type intent string

const (
	inOK       intent = "现有的行"
	inMissing  intent = "不存在的键"
	inStale    intent = "错的版本号"
	inNew      intent = "新的键"
	inDup      intent = "已有的键"
	inInvalid  intent = "不合法的请求"
	inOptional intent = "新的键(可选的输入不传)"
)

// intents: 请求序列的意图。每种至少 minOutcome+1 个(结果可能被别的原因改变: 例如新建的值违反唯一约束),其余随机。
func (e *cmdEnv) intents() []intent {
	var need, fill []intent
	switch e.cmd.Op {
	case "insert":
		need = []intent{inNew, inDup}
		fill = []intent{inNew, inNew, inDup}
		if e.hasOptional() {
			need = append(need, inOptional)
			fill = append(fill, inOptional)
		}
	default:
		need = []intent{inOK, inMissing}
		fill = []intent{inOK, inOK, inMissing}
		if e.locked {
			need = append(need, inStale)
			fill = append(fill, inStale)
		}
	}
	var out []intent
	for _, in := range need {
		for i := 0; i <= minOutcome+1; i++ {
			out = append(out, in)
		}
	}
	out = append(out, inInvalid, inInvalid)
	for len(out) < cmdRequests {
		out = append(out, fill[e.rnd.Intn(len(fill))])
	}
	e.rnd.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

func (e *cmdEnv) hasOptional() bool {
	for _, b := range e.cmd.Write {
		if f := e.api.Request.Field(b.Field); f != nil && f.Presence {
			return true
		}
	}
	return false
}

type stepResult struct {
	res     Result
	outcome command.Outcome
}

// step: 第 i 个请求: 造请求 → 模型 → 服务器 → 比较响应和状态。
func (e *cmdEnv) step(url string, i int, in intent) stepResult {
	now := e.t0.Add(time.Duration(i) * time.Minute)
	req, mreq := e.request(in, now)
	name := fmt.Sprintf("请求 #%02d %s", i+1, in)
	res := func(ok bool, detail string, o command.Outcome) stepResult {
		return stepResult{res: Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: ok, Detail: detail}, outcome: o}
	}
	if req == nil {
		return res(true, "跳过: 造不出这种请求(状态里没有合适的行)", "")
	}
	model := e.state.Clone()
	var want command.Result
	bad, err := e.val.invalid(req)
	if err != nil {
		return res(false, "请求: "+err.Error(), "")
	}
	if bad {
		want = command.Result{Outcome: command.Invalid}
	} else {
		want = model.Apply(e.cmd.Op, e.t.Name, mreq)
	}
	req[NowKey] = now.Format(time.RFC3339Nano)
	code, msg, got, err := callJSON(url, req, &e.api.Response)
	if err != nil {
		return res(false, err.Error(), want.Outcome)
	}
	var problems []string
	if code != want.Outcome.Code() {
		problems = append(problems, fmt.Sprintf("错误码: 模型 %s(%s),服务器 %s(%s)", want.Outcome.Code(), want.Outcome, code, msg))
	} else if code == "OK" {
		problems = append(problems, e.compareResponse(want.Row, got)...)
	}
	dbRows, err := command.ReadRows(e.ctx, e.conn, e.t)
	if err != nil {
		return res(false, err.Error(), want.Outcome)
	}
	if d := command.Diff(e.t, model.Tables[e.t.Name].Rows, dbRows, true); d != "" {
		problems = append(problems, "状态: "+d)
	}
	desc := fmt.Sprintf("%s → %s", e.describe(req), want.Outcome)
	if len(problems) > 0 {
		e.state.Tables[e.t.Name].Rows = dbRows // 同步成数据库的状态(后面的请求从实际的状态继续)
		return res(false, desc+" | "+strings.Join(problems, " | "), want.Outcome)
	}
	e.state = model
	return res(true, desc, want.Outcome)
}

// request: 按意图造请求(protojson 的写法)和模型的输入(规范值)。造不出来 → nil。
func (e *cmdEnv) request(in intent, now time.Time) (map[string]any, command.Request) {
	ts := e.state.Tables[e.t.Name]
	req := map[string]any{}
	mreq := command.Request{Key: command.Row{}, Write: command.Row{}, Set: command.Row{}}
	put := func(f *ir.Field, col string, v any, into command.Row) {
		c := e.t.Col(col)
		req[f.Name] = e.jsonValue(f, *c, v)
		cv, _ := command.Canon(c.Type, v)
		into[col] = cv
	}
	var target command.Row // 现有的行(更新 / 删除的对象,或者 已有的键)
	keyFromState := func() bool {
		if len(ts.Rows) == 0 {
			return false
		}
		target = ts.Rows[e.rnd.Intn(len(ts.Rows))]
		return true
	}
	missingKey := func() []any {
		var free [][]any
		for _, k := range e.keys {
			r := command.Row{}
			for j, col := range e.t.PK {
				r[col], _ = command.Canon(e.t.Col(col).Type, k[j])
			}
			if !e.exists(r) {
				free = append(free, k)
			}
		}
		if len(free) == 0 {
			return nil
		}
		return free[e.rnd.Intn(len(free))]
	}
	switch e.cmd.Op {
	case "insert":
		var key []any
		switch in {
		case inDup:
			if !keyFromState() {
				return nil, mreq
			}
			for _, col := range e.t.PK {
				key = append(key, rawOf(target[col]))
			}
		default:
			if key = missingKey(); key == nil {
				return nil, mreq
			}
		}
		for _, b := range e.cmd.Write {
			f := e.api.Request.Field(b.Field)
			if j := indexOf(e.t.PK, b.Column); j >= 0 {
				put(f, b.Column, key[j], mreq.Write)
				continue
			}
			if f.Presence && (in == inOptional || e.rnd.Intn(4) == 0) {
				continue // 不传 → 数据库的默认值
			}
			p := e.pools[b.Column]
			if len(p) == 0 {
				return nil, mreq
			}
			put(f, b.Column, p[e.rnd.Intn(len(p))], mreq.Write)
		}
	default:
		switch in {
		case inMissing:
			k := missingKey()
			if k == nil {
				return nil, mreq
			}
			target = command.Row{}
			for j, col := range e.t.PK {
				target[col] = k[j]
			}
			for _, b := range e.cmd.Key {
				put(e.api.Request.Field(b.Field), b.Column, k[indexOf(e.t.PK, b.Column)], mreq.Key)
			}
		default:
			if !keyFromState() {
				return nil, mreq
			}
			for _, b := range e.cmd.Key {
				put(e.api.Request.Field(b.Field), b.Column, rawOf(target[b.Column]), mreq.Key)
			}
		}
		for _, b := range e.cmd.Write {
			p := e.pools[b.Column]
			if len(p) == 0 {
				return nil, mreq
			}
			put(e.api.Request.Field(b.Field), b.Column, p[e.rnd.Intn(len(p))], mreq.Write)
		}
		if l := e.cmd.Lock; l != nil {
			f := e.api.Request.Field(l.Field)
			v := int64(1 + e.rnd.Intn(3))
			if cur, ok := target[l.Column].(string); ok {
				n, _ := strconv.ParseInt(cur, 10, 64)
				v = n
				if in == inStale { // 错的版本号: 旧的(读了之后被别人改过)或者更新的
					if n > 1 && e.rnd.Intn(2) == 0 {
						v = n - 1
					} else {
						v = n + 1 + int64(e.rnd.Intn(2))
					}
				}
			}
			c := e.t.Col(l.Column)
			req[f.Name] = e.jsonValue(f, *c, v)
			mreq.Lock, mreq.LockV = l.Column, strconv.FormatInt(v, 10)
		}
	}
	for _, s := range e.cmd.Set {
		var v any = s.Value
		if s.Now {
			v = now
		}
		cv, err := command.Canon(s.Type, v)
		if err != nil {
			return nil, mreq
		}
		mreq.Set[s.Column] = cv
	}
	if in == inInvalid && !e.breakRequest(req) {
		return nil, mreq
	}
	return req, mreq
}

// breakRequest: 把一个字段换成违反规则的值(约束的边界值)。没有能违反的规则 → false。
func (e *cmdEnv) breakRequest(req map[string]any) bool {
	for _, i := range e.rnd.Perm(len(e.api.Request.Fields)) {
		f := &e.api.Request.Fields[i]
		bs, _ := constraints.Boundaries(f, req)
		for _, b := range bs {
			if !b.Valid && !b.Unset {
				req[f.Name] = b.Value
				return true
			}
		}
	}
	return false
}

func (e *cmdEnv) exists(key command.Row) bool {
	for _, r := range e.state.Tables[e.t.Name].Rows {
		ok := true
		for c, v := range key {
			if !command.Equal(r[c], v) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// rawOf: 规范值 → 数据库的写法(整数的文本 → int64;其他 = 文本)。
func rawOf(v any) any {
	if s, ok := v.(string); ok {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(n, 10) == s {
			return n
		}
	}
	return v
}

func indexOf(xs []string, x string) int {
	for i, y := range xs {
		if y == x {
			return i
		}
	}
	return -1
}

// compareResponse: 响应的每个字段(= 写之后的行的列)vs 模型的行。
func (e *cmdEnv) compareResponse(row command.Row, got map[string]any) []string {
	var out []string
	for i, c := range e.cmd.Returning {
		f := &e.api.Response.Fields[i]
		want := row[c.Name]
		v, present := got[f.Name]
		var gv any
		var err error
		switch {
		case !present && f.Presence:
			gv = nil
		case !present: // protojson 省略零值
			gv, err = command.Canon(e.t.Col(c.Name).Type, zeroOf(c.Type))
		case f.Kind == "enum":
			gv = nil
			if en := e.plan.Enum(e.t.Name, c.Name); en != nil {
				for _, x := range en.Values {
					if x.Name == fmt.Sprint(v) {
						gv, err = command.Canon(e.t.Col(c.Name).Type, x.DB)
					}
				}
			}
			if gv == nil {
				gv = "<枚举 " + fmt.Sprint(v) + ">"
			}
		default:
			gv, err = command.Canon(e.t.Col(c.Name).Type, v)
		}
		if err != nil {
			out = append(out, fmt.Sprintf("响应.%s: %v", f.Name, err))
			continue
		}
		if !command.Equal(want, gv) {
			out = append(out, fmt.Sprintf("响应.%s: 模型 %s,服务器 %s", f.Name, command.Show(want), command.Show(gv)))
		}
	}
	return out
}

func zeroOf(sqlType string) any {
	switch sqlType {
	case "int2", "int4", "int8", "numeric":
		return "0"
	case "bool":
		return false
	case "timestamptz", "timestamp", "date":
		return nil
	}
	return ""
}

func (e *cmdEnv) describe(req map[string]any) string {
	keys := make([]string, 0, len(req))
	for k := range req {
		if k != NowKey {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, req[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// coverage: 覆盖条件: 每种可能的结果都出现 ≥ minOutcome 次(新建: 成功 / 唯一约束冲突;更新 / 删除: 成功 / 不存在
// (+ 版本冲突))。不满足 → 失败(变异测试靠这些结果抓 bug)。
func (e *cmdEnv) coverage(counts map[command.Outcome]int) Result {
	need := []command.Outcome{command.OK}
	switch {
	case e.cmd.Op == "insert":
		if len(e.t.Uniques) > 0 {
			need = append(need, command.Unique)
		}
	default:
		need = append(need, command.NotFound)
		if e.locked {
			need = append(need, command.Conflict)
		}
	}
	var parts, weak []string
	for _, o := range need {
		parts = append(parts, fmt.Sprintf("%s %d", o, counts[o]))
		if counts[o] < minOutcome {
			weak = append(weak, string(o))
		}
	}
	for _, o := range []command.Outcome{command.Invalid, command.NotNull, command.ForeignKey} {
		if counts[o] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", o, counts[o]))
		}
	}
	text := "结果: " + strings.Join(parts, " / ") + fmt.Sprintf("(初始 %d 行,%d 个请求)", e.rows0, cmdRequests)
	if len(weak) > 0 {
		text += fmt.Sprintf(" ✗ 少于 %d 次: %v", minOutcome, weak)
	}
	return Result{Spec: e.api.ID, Suite: "diff", Case: "覆盖条件", Passed: len(weak) == 0, Detail: text}
}
