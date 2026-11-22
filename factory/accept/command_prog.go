package accept

// 命令工厂 R3-2 的验证(设计稿 §5.2–§5.5),对象 = 操作的列表(组合命令原样;新建 / 按键更新 / 按键删除 = 一个操作):
//   - 状态差分(组合命令): 随机的多表初始状态 + 随机的请求序列;每个请求之后比较 响应(含错误码、业务原因码)
//     + 全部相关的表,模型(factory/command 的 State.Run)vs 数据库。覆盖条件: 每个操作的成功和每种失败
//     (不存在 / 版本冲突 / 条件不满足 / 唯一约束冲突;幂等新建的 新建 / 命中)都出现 ≥ minOutcome 次。
//     请求按"目标"抽样: 对每个目标随机造请求,用模型判断结果,取第一个命中目标的(拒绝抽样)。
//   - 故障注入(§5.3): 第 k 个写操作之后强制失败(请求头 X-Factory-Fail-After,服务器 FACTORY_TEST_FAULTS=1),
//     每个 k = 1..操作数: 响应是错误,数据库的状态 = 请求之前。
//   - 并发(§5.4): 同一个请求同时发 N 个: 成功次数 ≤ 串行执行的成功次数(理论上限),之后的状态 = 串行执行 s 次的状态
//     (条件里的不等式、版本号、幂等键的唯一都由状态的比较检查);乐观锁另外跑"读版本 → 写 → 冲突就重读"的循环,
//     版本号 = 初始 + 成功次数(没有丢失的更新)。幂等新建: 所有请求都成功,只有一个 新建 = true,其余的输出相同。
//   - 重放(§5.5): 只有幂等新建的命令,同一个请求发 3 次: 结果(除了 新建)和状态与发 1 次相同。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"math/rand"

	"github.com/jackc/pgx/v5"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"

	"apifactory/factory/command"
	"apifactory/factory/compiler"
	"apifactory/factory/constraints"
	"apifactory/factory/ir"
	"apifactory/factory/protoload"
)

const (
	progSamples   = 80 // 一个目标最多抽样几次
	concurrentN   = 8  // 并发的请求数
	lockWorkers   = 4  // 乐观锁的循环: 几个并发的写入方
	lockSuccesses = 2  // 每个写入方要成功几次
)

type progEnv struct {
	ctx      context.Context
	conn     *pgx.Conn
	url      string
	plan     *ir.Plan
	api      *ir.API
	ops      []ir.CommandOp
	rnd      *rand.Rand
	val      validator
	state    *command.State
	tables   []string                  // 模型里的表(外键的顺序: 被引用的在前)
	cat      map[string]*command.Table //
	helper   map[string]*cmdEnv        // 每张表: 值域 / 主键的候选(R3-1 的造数据)
	keys     map[string][][]any        // 每张表: 主键的候选(数据库的写法)
	home     map[string][2]string      // 请求字段 → 写进的 表.列(值域、枚举的映射)
	base     map[string]any
	t0       time.Time
	seq      int
	unmetMsg map[string]bool
	msg      *msgEnv // 有 发出 的组合命令(R4): outbox
}

// runProg: 一个命令的 R3-2 验证。diff = 是否跑状态差分(组合命令;R3-1 的命令有自己的差分,command_diff.go)。
func runProg(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API, diff bool) []Result {
	e, fail := newProgEnv(ctx, d, url, plan, api)
	if fail != nil {
		return fail
	}
	var results []Result
	if diff {
		results = append(results, e.diff()...)
	}
	// 每组测试从新的随机初始状态开始: 前面的请求可能用完了某种行(例: 重新上架之后没有已下架的商品了)
	for _, suite := range []func() []Result{e.faults, e.concurrency, e.replay} {
		if err := e.load(); err != nil {
			return append(results, Result{Spec: api.ID, Suite: "diff", Case: "R3-2 准备", Passed: false, Detail: "初始状态: " + err.Error()})
		}
		results = append(results, suite()...)
	}
	return results
}

// newProgEnv: 验证的环境(随机的初始状态已经灌好)。失败 → 结果。
func newProgEnv(ctx context.Context, d *dbs, url string, plan *ir.Plan, api *ir.API) (*progEnv, []Result) {
	fail := func(format string, args ...any) []Result {
		return []Result{{Spec: api.ID, Suite: "diff", Case: "R3-2 准备", Passed: false, Detail: fmt.Sprintf(format, args...)}}
	}
	h := fnv.New64a()
	h.Write([]byte(api.ID + "/prog" + os.Getenv("FACTORY_DIFF_SEED")))
	e := &progEnv{ctx: ctx, conn: d.main, url: url, plan: plan, api: api, ops: command.OpsOf(api.Command),
		rnd: rand.New(rand.NewSource(int64(h.Sum64()))), cat: map[string]*command.Table{}, helper: map[string]*cmdEnv{},
		keys: map[string][][]any{}, home: map[string][2]string{}, base: api.BaseRequest, t0: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	if len(api.Command.Emits) > 0 { // 发出(R4): 每个请求新增的 outbox 行和模型比较
		e.msg = newMsgEnv(ctx, d, url, api)
	}
	files, err := protoload.Load()
	if err != nil {
		return nil, fail("proto: %v", err)
	}
	md, err := protoload.FindMethod(files, api.ProtoPackage, api.Service, api.Method)
	if err != nil {
		return nil, fail("proto: %v", err)
	}
	e.val = validator{md: md.Input()}
	if _, err := e.conn.Exec(ctx, "SET TimeZone = 'UTC'"); err != nil {
		return nil, fail("%v", err)
	}
	if err := e.load(); err != nil {
		return nil, fail("初始状态: %v", err)
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// 初始状态
// ---------------------------------------------------------------------------

func (e *progEnv) load() error {
	refs, err := foreignRefs(e.ctx, e.conn)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, op := range e.ops {
		names[op.Table] = true
	}
	// 外键相关的表: 写的表引用的(递归)+ 引用写的表的(删除的检查)
	for changed := true; changed; {
		changed = false
		for t := range names {
			for _, p := range refs[t] {
				if !names[p] {
					names[p], changed = true, true
				}
			}
		}
	}
	for child, ps := range refs {
		for _, p := range ps {
			if names[p] && !names[child] {
				for _, op := range e.ops {
					if op.Table == p && op.Kind == "delete" {
						names[child] = true
					}
				}
			}
		}
	}
	var list []string
	for t := range names {
		list = append(list, t)
	}
	e.tables = sortedTables(refs, list)
	var idents []string
	for _, t := range e.tables {
		ct, err := command.LoadTable(e.ctx, e.conn, t)
		if err != nil {
			return err
		}
		e.cat[t] = ct
		idents = append(idents, pgx.Identifier{t}.Sanitize())
	}
	if _, err := e.conn.Exec(e.ctx, "TRUNCATE "+strings.Join(idents, ", ")+" RESTART IDENTITY CASCADE"); err != nil {
		return err
	}
	if e.msg != nil {
		if err := e.msg.reset(); err != nil {
			return err
		}
	}
	// 请求字段 → 列(第一个写进的列)
	byCol := map[string]map[string]*ir.Field{}
	bind := func(table, col string, x *ir.CmdExpr) {
		if x == nil || x.Op != "input" {
			return
		}
		f := e.api.Request.Field(x.Name)
		if f == nil {
			return
		}
		if byCol[table] == nil {
			byCol[table] = map[string]*ir.Field{}
		}
		if byCol[table][col] == nil {
			byCol[table][col] = f
		}
		if _, ok := e.home[x.Name]; !ok {
			e.home[x.Name] = [2]string{table, col}
		}
	}
	for _, op := range e.ops {
		for _, v := range op.Key {
			bind(op.Table, v.Column, v.Expr)
		}
		for _, v := range op.Values {
			bind(op.Table, v.Column, v.Expr)
		}
		if op.Lock != nil {
			bind(op.Table, op.Lock.Column, op.Lock.Expr)
		}
	}
	for _, op := range e.ops { // 只出现在条件里的输入(和列比较): 值域跟着那一列
		if op.Cond == nil {
			continue
		}
		walkExpr(op.Cond, func(x *ir.CmdExpr) {
			if (x.Op == "=" || x.Op == "<>") && x.Args[0].Op == "col" && x.Args[1].Op == "input" {
				bind(op.Table, x.Args[0].Name, x.Args[1])
			}
		})
	}
	e.state = &command.State{Tables: map[string]*command.TableState{}}
	for _, t := range e.tables {
		ct := e.cat[t]
		h := &cmdEnv{ctx: e.ctx, conn: e.conn, plan: e.plan, api: e.api, t: ct, rnd: e.rnd, val: e.val,
			byCol: map[string]*ir.Field{}, pools: map[string][]any{}, base: e.base}
		for c, f := range byCol[t] {
			h.byCol[c] = f
		}
		for _, c := range ct.Cols {
			h.pools[c.Name] = h.poolFor(c)
		}
		for _, op := range e.ops { // 条件里的整数列: 值域加上 0(余额 / 库存用完的行,条件不满足要出现)
			if op.Table != t || op.Cond == nil {
				continue
			}
			walkExpr(op.Cond, func(x *ir.CmdExpr) {
				if x.Op == "col" && isIntSQL(x.Type) && byCol[t][x.Name] == nil {
					h.pools[x.Name] = append(h.pools[x.Name], int64(0), int64(0))
				}
			})
		}
		e.helper[t] = h
		generated := len(ct.PK) == 1 && ct.Col(ct.PK[0]).Default != nil && ct.Col(ct.PK[0]).Default.Generated
		if !generated {
			e.keys[t] = h.keySpace(cmdInitialRows * 6) // 新建 / 幂等新建要很多新的键(差分 + 故障注入 + 并发 + 重放)
		}
		n := cmdInitialRows
		perm := e.rnd.Perm(max(len(e.keys[t]), n))
		for i := 0; i < n; i++ {
			row := map[string]any{}
			if !generated {
				if i >= len(e.keys[t]) {
					break
				}
				for j, k := range ct.PK {
					row[k] = e.keys[t][perm[i]%len(e.keys[t])][j]
				}
			}
			fkCols := map[string]bool{}
			okFK := true
			for _, fk := range ct.FKs { // 外键: 取被引用的表里已有的行
				parent := e.state.Tables[fk.RefTable]
				if parent == nil || len(parent.Rows) == 0 {
					okFK = false
					break
				}
				pr := parent.Rows[e.rnd.Intn(len(parent.Rows))]
				for j, c := range fk.Cols {
					row[c] = rawOf(pr[fk.RefCols[j]])
					fkCols[c] = true
				}
			}
			if !okFK {
				continue
			}
			for _, c := range ct.Cols {
				if _, set := row[c.Name]; set || fkCols[c.Name] || (c.Default != nil && c.Default.Generated && contains(ct.PK, c.Name)) {
					continue
				}
				if p := h.pools[c.Name]; len(p) > 0 && (c.NotNull || e.rnd.Intn(4) > 0) {
					row[c.Name] = p[e.rnd.Intn(len(p))]
				}
			}
			_ = h.insertRaw(row) // 违反约束的被拒绝: 跳过
		}
		if generated { // 序号各表错开(不然两张表的新的 id 总是一样,"取错了操作的 id"测不出来)
			skip := 10*(len(e.state.Tables)+1) + e.rnd.Intn(7)
			q := fmt.Sprintf("SELECT setval(pg_get_serial_sequence($1, $2), coalesce((SELECT max(%s) FROM %s), 0) + %d)",
				pgx.Identifier{ct.PK[0]}.Sanitize(), pgx.Identifier{t}.Sanitize(), skip)
			if _, err := e.conn.Exec(e.ctx, q, pgx.Identifier{t}.Sanitize(), ct.PK[0]); err != nil {
				return err
			}
		}
		rows, err := command.ReadRows(e.ctx, e.conn, ct)
		if err != nil {
			return err
		}
		e.state.Tables[t] = &command.TableState{T: ct, Rows: rows}
	}
	return nil
}

func contains(xs []string, x string) bool { return indexOf(xs, x) >= 0 }

func walkExpr(x *ir.CmdExpr, f func(*ir.CmdExpr)) {
	if x == nil {
		return
	}
	f(x)
	for _, a := range x.Args {
		walkExpr(a, f)
	}
}

// ---------------------------------------------------------------------------
// 请求的抽样
// ---------------------------------------------------------------------------

// sample: 一个随机的请求(protojson 的写法)。键 ~65% 取现有的行,版本号 ~70% 对,条件里的输入取值域 + 边界值。
func (e *progEnv) sample() map[string]any {
	req := map[string]any{}
	set := func(name string, v any) {
		if _, done := req[name]; !done {
			if f := e.api.Request.Field(name); f != nil {
				req[name] = e.jsonOf(f, v)
			}
		}
	}
	for _, op := range e.ops {
		ts := e.state.Tables[op.Table]
		var target command.Row
		switch op.Kind {
		case "update", "delete":
			var keyIns []ir.CommandVal
			for _, k := range op.Key {
				if k.Expr.Op == "input" {
					keyIns = append(keyIns, k)
				}
			}
			assigned := len(keyIns) > 0
			for _, k := range keyIns {
				if _, ok := req[k.Expr.Name]; !ok {
					assigned = false
				}
			}
			switch {
			case assigned: // 前面的操作已经决定了键: 找那一行
				key := command.Row{}
				for _, k := range keyIns {
					key[k.Column], _ = command.Canon(k.Type, req[k.Expr.Name])
				}
				target = findRow(ts, key)
			case len(keyIns) > 0 && len(ts.Rows) > 0 && e.rnd.Intn(100) < 65:
				target = ts.Rows[e.rnd.Intn(len(ts.Rows))]
				for _, k := range keyIns {
					set(k.Expr.Name, rawOf(target[k.Column]))
				}
			case len(keyIns) > 0:
				if k := e.missingKey(op.Table); k != nil {
					for _, kv := range keyIns {
						set(kv.Expr.Name, k[indexOf(ts.T.PK, kv.Column)])
					}
				}
			}
			if op.Lock != nil && op.Lock.Expr.Op == "input" {
				v := int64(1 + e.rnd.Intn(3))
				if target != nil {
					n, _ := strconv.ParseInt(fmt.Sprint(target[op.Lock.Column]), 10, 64)
					v = n
					if e.rnd.Intn(100) >= 70 {
						if n > 1 && e.rnd.Intn(2) == 0 {
							v = n - 1
						} else {
							v = n + 1 + int64(e.rnd.Intn(2))
						}
					}
				}
				set(op.Lock.Expr.Name, v)
			}
			if op.Cond != nil {
				for _, name := range command.InputNames(op.Cond) {
					if c := e.condCandidates(name, op, target); len(c) > 0 {
						set(name, c[e.rnd.Intn(len(c))])
					}
				}
			}
		case "insert", "insert_idempotent":
			uniq := uniqueCols(ts.T)
			var keyRow command.Row // 唯一键的列一起取: 已有的行(冲突 / 命中)或者新的键
			if len(ts.Rows) > 0 && e.rnd.Intn(100) < 40 {
				keyRow = ts.Rows[e.rnd.Intn(len(ts.Rows))]
			}
			var fresh []any
			if keyRow == nil {
				fresh = e.missingKey(op.Table)
			}
			for _, v := range op.Values {
				if v.Expr.Op != "input" {
					continue
				}
				f := e.api.Request.Field(v.Expr.Name)
				switch {
				case uniq[v.Column] && keyRow != nil:
					set(v.Expr.Name, rawOf(keyRow[v.Column]))
				case uniq[v.Column] && fresh != nil && indexOf(ts.T.PK, v.Column) >= 0:
					set(v.Expr.Name, fresh[indexOf(ts.T.PK, v.Column)])
				case f != nil && f.Presence && e.rnd.Intn(4) == 0:
					req[v.Expr.Name] = nil // 可以不传的输入: 不传(下面删掉)
				default:
					if p := e.helper[op.Table].pools[v.Column]; len(p) > 0 {
						set(v.Expr.Name, p[e.rnd.Intn(len(p))])
					}
				}
			}
		}
	}
	for _, f := range e.api.Request.Fields { // 剩下的字段: 写进的列的值域,或者基础请求
		if _, done := req[f.Name]; done {
			continue
		}
		if h, ok := e.home[f.Name]; ok {
			if p := e.helper[h[0]].pools[h[1]]; len(p) > 0 {
				set(f.Name, p[e.rnd.Intn(len(p))])
				continue
			}
		}
		if v, ok := e.base[f.Name]; ok {
			req[f.Name] = v
		}
	}
	for k, v := range req {
		if v == nil {
			delete(req, k)
		}
	}
	return req
}

func findRow(ts *command.TableState, key command.Row) command.Row {
	for _, r := range ts.Rows {
		ok := len(key) > 0
		for c, v := range key {
			if v == nil || !command.Equal(r[c], v) {
				ok = false
			}
		}
		if ok {
			return r
		}
	}
	return nil
}

// uniqueCols: 主键 / 唯一键里的列。
func uniqueCols(t *command.Table) map[string]bool {
	out := map[string]bool{}
	for _, u := range t.Uniques {
		for _, c := range u {
			out[c] = true
		}
	}
	return out
}

func (e *progEnv) missingKey(table string) []any {
	ts := e.state.Tables[table]
	var free [][]any
	for _, k := range e.keys[table] {
		r := command.Row{}
		for j, col := range ts.T.PK {
			r[col], _ = command.Canon(ts.T.Col(col).Type, k[j])
		}
		if findRow(ts, r) == nil {
			free = append(free, k)
		}
	}
	if len(free) == 0 {
		return nil
	}
	return free[e.rnd.Intn(len(free))]
}

// condCandidates: 条件里的输入的候选值: 值域 + 让条件正好在边界上(± 1)的值(按对象的行的列和条件里的常量)。
func (e *progEnv) condCandidates(name string, op ir.CommandOp, target command.Row) []any {
	f := e.api.Request.Field(name)
	if f == nil {
		return nil
	}
	var cands []any
	if h, ok := e.home[name]; ok {
		cands = append(cands, e.helper[h[0]].pools[h[1]]...)
	}
	if d := constraints.DomainOf(f); d != nil {
		for _, v := range d.Values {
			cands = append(cands, dbForm("", v))
		}
	}
	typ, _ := compiler.InputSQLType(f)
	if isIntSQL(typ) || typ == "numeric" {
		var anchors []*big.Rat
		walkExpr(op.Cond, func(x *ir.CmdExpr) {
			switch x.Op {
			case "col":
				if target != nil {
					if r, ok := new(big.Rat).SetString(fmt.Sprint(target[x.Name])); ok {
						anchors = append(anchors, r)
					}
				}
			case "const":
				if r, ok := new(big.Rat).SetString(x.Value); ok {
					anchors = append(anchors, r)
				}
			}
		})
		anchors = append(anchors, new(big.Rat))
		for _, a := range anchors {
			for _, b := range anchors {
				for _, d := range []int64{-1, 0, 1} {
					for _, sign := range []int64{1, -1} {
						v := new(big.Rat).Sub(a, b)
						v.Mul(v, big.NewRat(sign, 1))
						v.Add(v, big.NewRat(d, 1))
						if isIntSQL(typ) {
							if !v.IsInt() {
								continue
							}
							cands = append(cands, v.Num().Int64())
						} else {
							cands = append(cands, ratText(v))
						}
					}
				}
			}
		}
	}
	var ok []any
	seen := map[string]bool{}
	for _, v := range cands {
		k := fmt.Sprint(v)
		if seen[k] {
			continue
		}
		seen[k] = true
		req := map[string]any{}
		for bk, bv := range e.base {
			req[bk] = bv
		}
		req[name] = e.jsonOf(f, v)
		if bad, err := e.val.invalid(req); err == nil && !bad {
			ok = append(ok, v)
		}
	}
	return ok
}

func isIntSQL(t string) bool { return t == "int2" || t == "int4" || t == "int8" }

// jsonOf: 数据库的写法 → 请求里的写法(protojson)。
func (e *progEnv) jsonOf(f *ir.Field, v any) any {
	if f.Kind == "enum" {
		if h, ok := e.home[f.Name]; ok {
			if en := e.plan.Enum(h[0], h[1]); en != nil {
				for _, x := range en.Values {
					if fmt.Sprint(x.DB) == fmt.Sprint(v) {
						return x.Name
					}
				}
			}
		}
		return v
	}
	switch x := v.(type) {
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case int64:
		if f.Kind == "int64" || f.Kind == "uint64" {
			return strconv.FormatInt(x, 10)
		}
		return x
	case string:
		switch f.Kind {
		case "bool":
			return x == "true"
		case "int32", "uint32":
			n, err := strconv.ParseInt(x, 10, 64)
			if err == nil {
				return n
			}
		}
		if strings.Contains(x, "/") { // 十进制的规范形(分数)→ 小数的文本
			if r, ok := new(big.Rat).SetString(x); ok {
				return ratText(r)
			}
		}
	}
	return v
}

// inputs: 请求 → 模型的输入(枚举的名字 → 数据库值)。
func (e *progEnv) inputs(req map[string]any) command.Inputs {
	in := command.Inputs{}
	for k, v := range req {
		if k == NowKey {
			continue
		}
		f := e.api.Request.Field(k)
		if f != nil && f.Kind == "enum" {
			if h, ok := e.home[k]; ok {
				if en := e.plan.Enum(h[0], h[1]); en != nil {
					for _, x := range en.Values {
						if x.Name == fmt.Sprint(v) {
							v = x.DB
						}
					}
				}
			}
		}
		in[k] = v
	}
	return in
}

// ---------------------------------------------------------------------------
// 一个请求
// ---------------------------------------------------------------------------

type progAim struct {
	op  int
	out command.Outcome
}

func (a progAim) String() string {
	if a.op < 0 {
		return string(a.out)
	}
	return fmt.Sprintf("%d:%s", a.op+1, a.out)
}

// hits: 结果命中目标吗。op 的 成功 = 这个操作执行成功了(后面的操作可能失败)。
func hits(r command.ProgResult, a progAim) bool {
	switch a.out {
	case command.OK:
		if a.op < 0 {
			return r.Outcome == command.OK
		}
		return len(r.Ops) > a.op && r.Ops[a.op].Ran
	case command.Created, command.Hit:
		return r.Outcome == command.OK && len(r.Ops) > a.op && r.Ops[a.op].Created == (a.out == command.Created)
	case command.Invalid:
		return r.Outcome == command.Invalid
	}
	return r.Failed == a.op && r.Outcome == a.out
}

// find: 抽样到命中目标的请求(没有 → nil)。
func (e *progEnv) find(a progAim) map[string]any {
	for i := 0; i < progSamples; i++ {
		req := e.sample()
		if a.out == command.Invalid {
			if !e.breakReq(req) {
				return nil
			}
			return req
		}
		if bad, err := e.val.invalid(req); err != nil || bad {
			continue
		}
		if hits(e.state.Clone().Run(e.ops, e.inputs(req), e.now()), a) {
			return req
		}
	}
	return nil
}

func (e *progEnv) breakReq(req map[string]any) bool {
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

func (e *progEnv) now() any {
	v, _ := command.Canon("timestamptz", e.t0.Add(time.Duration(e.seq)*time.Minute))
	return v
}

func (e *progEnv) nowText() string {
	return e.t0.Add(time.Duration(e.seq) * time.Minute).Format(time.RFC3339Nano)
}

// reply: 服务器的回答。
type reply struct {
	code, msg, reason string
	got               map[string]any
	err               error
}

// call: 发一个请求(Connect 的 HTTP/JSON)。fault > 0 = 第 fault 个写操作之后强制失败。
func (e *progEnv) call(req map[string]any, now string, fault int) reply {
	body, _ := json.Marshal(withoutNow(req))
	hr, _ := http.NewRequest(http.MethodPost, e.url, bytes.NewReader(body))
	hr.Header.Set(NowHeader, now)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Connect-Protocol-Version", "1")
	if fault > 0 {
		hr.Header.Set("X-Factory-Fail-After", strconv.Itoa(fault))
	}
	if t := os.Getenv("API_TOKEN"); t != "" {
		hr.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return reply{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		var x struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"details"`
		}
		_ = json.Unmarshal(raw, &x)
		r := reply{code: strings.ToUpper(x.Code), msg: x.Message}
		for _, d := range x.Details {
			if d.Type != "google.rpc.ErrorInfo" {
				continue
			}
			b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(d.Value, "="))
			var info errdetails.ErrorInfo
			if err == nil && proto.Unmarshal(b, &info) == nil {
				r.reason = info.Reason
			}
		}
		return r
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded map[string]any
	if err := dec.Decode(&decoded); err != nil {
		return reply{err: fmt.Errorf("bad json: %s", raw)}
	}
	got, _ := protoKeys(decoded, &e.api.Response).(map[string]any)
	return reply{code: "OK", got: got}
}

// readState: 数据库里全部相关的表。
func (e *progEnv) readState() (map[string][]command.Row, error) {
	out := map[string][]command.Row{}
	for _, t := range e.tables {
		rows, err := command.ReadRows(e.ctx, e.conn, e.cat[t])
		if err != nil {
			return nil, err
		}
		out[t] = rows
	}
	return out, nil
}

// diffState: 模型 vs 数据库(adopt = 模型采用数据库生成的值)。
func (e *progEnv) diffState(m *command.State, db map[string][]command.Row, adopt bool) []string {
	var out []string
	for _, t := range e.tables {
		if d := command.Diff(e.cat[t], m.Tables[t].Rows, db[t], adopt); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func (e *progEnv) syncState(db map[string][]command.Row) {
	for _, t := range e.tables {
		e.state.Tables[t].Rows = db[t]
	}
}

// checkReply: 回答 vs 模型的结果(错误码、业务原因码、输出)。
func (e *progEnv) checkReply(want command.ProgResult, r reply) []string {
	var out []string
	if r.err != nil {
		return []string{r.err.Error()}
	}
	if r.code != want.Code() {
		return []string{fmt.Sprintf("错误码: 模型 %s(%s %s),服务器 %s(%s)", want.Code(), want.Outcome, want.Detail, r.code, r.msg)}
	}
	if want.Outcome == command.Unmet {
		if r.reason != want.Reason {
			out = append(out, fmt.Sprintf("业务原因码: 模型 %s,服务器 ErrorInfo.reason %q", want.Reason, r.reason))
		}
		if !strings.HasPrefix(r.msg, want.Reason+":") {
			out = append(out, fmt.Sprintf("消息要以业务原因码开头: %q", r.msg))
		}
	}
	if r.code == "OK" {
		out = append(out, e.compareOutputs(want, r.got)...)
	}
	return out
}

// outputs: 响应字段的来源(组合命令 = Outputs;R3-1 的命令 = 写之后的行的同名列)。
func (e *progEnv) outputs() []ir.CommandOut {
	c := e.api.Command
	if c.Op == "command" {
		return c.Outputs
	}
	var out []ir.CommandOut
	for i, col := range c.Returning {
		out = append(out, ir.CommandOut{Field: e.api.Response.Fields[i].Name, Op: c.Op, Table: c.Table, Column: col.Name, Type: col.Type, NotNull: col.NotNull})
	}
	return out
}

func (e *progEnv) compareOutputs(want command.ProgResult, got map[string]any) []string {
	var out []string
	idx := map[string]int{}
	for i, op := range e.ops {
		idx[op.Name] = i
	}
	for i, o := range e.outputs() {
		f := &e.api.Response.Fields[i]
		j := idx[o.Op]
		if j >= len(want.Ops) {
			out = append(out, fmt.Sprintf("响应.%s: 模型没有操作 %s 的结果", f.Name, o.Op))
			continue
		}
		v, present := got[f.Name]
		if o.Created {
			gb := present && fmt.Sprint(v) == "true"
			if gb != want.Ops[j].Created {
				out = append(out, fmt.Sprintf("响应.%s(新建): 模型 %v,服务器 %v", f.Name, want.Ops[j].Created, gb))
			}
			continue
		}
		wv := want.Ops[j].Row[o.Column]
		ct := e.cat[o.Table].Col(o.Column)
		var gv any
		var err error
		switch {
		case !present && f.Presence:
			gv = nil
		case !present:
			gv, err = command.Canon(ct.Type, zeroOf(o.Type))
		case f.Kind == "enum":
			gv = "<枚举 " + fmt.Sprint(v) + ">"
			if en := e.plan.Enum(o.Table, o.Column); en != nil {
				for _, x := range en.Values {
					if x.Name == fmt.Sprint(v) {
						gv, err = command.Canon(ct.Type, x.DB)
					}
				}
			}
		default:
			gv, err = command.Canon(ct.Type, v)
		}
		if err != nil {
			out = append(out, fmt.Sprintf("响应.%s: %v", f.Name, err))
			continue
		}
		if !command.Equal(wv, gv) {
			out = append(out, fmt.Sprintf("响应.%s: 模型 %s,服务器 %s", f.Name, command.Show(wv), command.Show(gv)))
		}
	}
	return out
}

// step: 一个请求: 模型 → 服务器 → 状态(采用数据库生成的值)→ 响应。不一致 → 模型同步成数据库的状态。
func (e *progEnv) step(name string, req map[string]any) (Result, command.ProgResult, reply) {
	e.seq++
	model := e.state.Clone()
	var want command.ProgResult
	bad, err := e.val.invalid(req)
	if err != nil {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: "请求: " + err.Error()}, want, reply{}
	}
	if bad {
		want = command.ProgResult{Outcome: command.Invalid, Failed: -1}
	} else {
		want = model.Run(e.ops, e.inputs(req), e.now())
	}
	if e.msg != nil {
		_, _ = e.msg.newRows() // 前面不检查的请求(并发 …)发出的
	}
	r := e.call(req, e.nowText(), 0)
	db, err := e.readState()
	if err != nil {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()}, want, r
	}
	problems := e.diffState(model, db, true)
	for i := range problems {
		problems[i] = "状态: " + problems[i]
	}
	if e.msg != nil { // 发出(R4): 成功 = 模型的消息(和写入同一个事务),失败 = 没有消息
		var emits []command.Msg
		if want.Outcome == command.OK {
			emits = command.RunEmits(e.api.Command, e.inputs(req), e.now(), want.Ops)
		}
		rows, err := e.msg.newRows()
		if err != nil {
			problems = append(problems, err.Error())
		}
		problems = append(problems, e.msg.checkRows(emits, rows)...)
	}
	problems = append(e.checkReply(want, r), problems...)
	desc := fmt.Sprintf("%s → %s", describeReq(req), outcomeText(want))
	if len(problems) > 0 {
		e.syncState(db)
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: desc + " | " + strings.Join(problems, " | ")}, want, r
	}
	e.state = model
	return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: true, Detail: desc}, want, r
}

func outcomeText(r command.ProgResult) string {
	switch {
	case r.Outcome == command.Unmet:
		return fmt.Sprintf("第 %d 个操作 %s(%s)", r.Failed+1, r.Outcome, r.Reason)
	case r.Failed >= 0:
		return fmt.Sprintf("第 %d 个操作 %s", r.Failed+1, r.Outcome)
	}
	var tags []string
	for i, o := range r.Ops {
		if o.Created {
			tags = append(tags, fmt.Sprintf("%d:新建", i+1))
		} else if o.Ran && o.Row != nil && len(r.Ops) > 0 {
			continue
		}
	}
	if len(tags) > 0 {
		return "成功(" + strings.Join(tags, " ") + ")"
	}
	return string(r.Outcome)
}

func describeReq(req map[string]any) string {
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

// ---------------------------------------------------------------------------
// 状态差分(组合命令)
// ---------------------------------------------------------------------------

// needs: 覆盖条件 = 每个操作要出现的结果。
func (e *progEnv) needs() []progAim {
	var out []progAim
	for i, op := range e.ops {
		out = append(out, progAim{i, command.OK})
		switch op.Kind {
		case "update", "delete":
			for _, k := range op.Key {
				if k.Expr.Op == "input" {
					out = append(out, progAim{i, command.NotFound})
					break
				}
			}
			if op.Lock != nil {
				out = append(out, progAim{i, command.Conflict})
			}
			if op.Cond != nil {
				out = append(out, progAim{i, command.Unmet})
			}
		case "insert_idempotent":
			out = append(out, progAim{i, command.Created}, progAim{i, command.Hit})
		case "insert":
			if e.inputUnique(op) {
				out = append(out, progAim{i, command.Unique})
			}
		}
	}
	return out
}

// inputUnique: 新建的某个唯一键的列全部来自输入(重复的键会冲突)。
func (e *progEnv) inputUnique(op ir.CommandOp) bool {
	fromInput := map[string]bool{}
	for _, v := range op.Values {
		if v.Expr.Op == "input" {
			fromInput[v.Column] = true
		}
	}
	for _, u := range e.cat[op.Table].Uniques {
		all := true
		for _, c := range u {
			all = all && fromInput[c]
		}
		if all {
			return true
		}
	}
	return false
}

func (e *progEnv) diff() []Result {
	needs := e.needs()
	var aims []progAim
	for _, a := range needs {
		for i := 0; i <= minOutcome+1; i++ {
			aims = append(aims, a)
		}
	}
	aims = append(aims, progAim{-1, command.Invalid}, progAim{-1, command.Invalid})
	total := max(cmdRequests, len(aims)+8)
	for len(aims) < total {
		aims = append(aims, needs[e.rnd.Intn(len(needs))])
	}
	e.rnd.Shuffle(len(aims), func(i, j int) { aims[i], aims[j] = aims[j], aims[i] })
	counts := map[progAim]int{}
	var results []Result
	for i, a := range aims {
		name := fmt.Sprintf("请求 #%02d 目标 %s", i+1, a)
		req := e.find(a)
		if req == nil { // 状态里没有合适的行了: 换一份新的随机初始状态再试
			if err := e.load(); err != nil {
				results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: "初始状态: " + err.Error()})
				continue
			}
			req = e.find(a)
		}
		if req == nil {
			results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: true, Detail: "跳过: 造不出命中目标的请求"})
			continue
		}
		res, want, _ := e.step(name, req)
		results = append(results, res)
		if want.Outcome == command.Invalid {
			counts[progAim{-1, command.Invalid}]++
			continue
		}
		for k, o := range want.Ops {
			if o.Ran {
				counts[progAim{k, command.OK}]++
			}
			if e.ops[k].Kind == "insert_idempotent" {
				if o.Created {
					counts[progAim{k, command.Created}]++
				} else {
					counts[progAim{k, command.Hit}]++
				}
			}
		}
		if want.Failed >= 0 {
			counts[progAim{want.Failed, want.Outcome}]++
		}
	}
	var parts, weak []string
	for _, a := range needs {
		parts = append(parts, fmt.Sprintf("%s(%s) %d", a, e.ops[a.op].Name, counts[a]))
		if counts[a] < minOutcome {
			weak = append(weak, a.String())
		}
	}
	text := fmt.Sprintf("结果: %s(%d 个请求)", strings.Join(parts, " / "), len(aims))
	if len(weak) > 0 {
		text += fmt.Sprintf(" ✗ 少于 %d 次: %v", minOutcome, weak)
	}
	return append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "覆盖条件(组合命令)", Passed: len(weak) == 0, Detail: text})
}

// ---------------------------------------------------------------------------
// 故障注入(§5.3)
// ---------------------------------------------------------------------------

func (e *progEnv) faults() []Result {
	var results []Result
	for k := 1; k <= len(e.ops)+len(e.api.Command.Emits); k++ { // 发出: outbox 的每一行也是一个写操作
		for rep := 1; rep <= 2; rep++ {
			name := fmt.Sprintf("故障注入: 第 %d 个写操作之后失败 #%d", k, rep)
			req := e.find(progAim{-1, command.OK})
			if req == nil {
				results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: "造不出全部成功的请求"})
				continue
			}
			e.seq++
			if e.msg != nil {
				_, _ = e.msg.newRows()
			}
			r := e.call(req, e.nowText(), k)
			db, err := e.readState()
			if err != nil {
				results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: err.Error()})
				continue
			}
			var problems []string
			switch {
			case r.err != nil:
				problems = append(problems, r.err.Error())
			case r.code == "OK":
				problems = append(problems, "响应是成功(故障没有注入: 服务器要 FACTORY_TEST_FAULTS=1)")
			case r.code != "INTERNAL":
				problems = append(problems, fmt.Sprintf("错误码 %s(%s),期待 INTERNAL", r.code, r.msg))
			}
			if d := e.diffState(e.state, db, false); len(d) > 0 {
				problems = append(problems, "状态应该不变(事务回滚): "+strings.Join(d, "; "))
				e.syncState(db)
			}
			if e.msg != nil { // 发出(R4): 回滚 = outbox 里也没有
				if rows, _ := e.msg.newRows(); len(rows) > 0 {
					problems = append(problems, fmt.Sprintf("回滚了却有 %d 条消息进了 outbox", len(rows)))
				}
			}
			results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: len(problems) == 0,
				Detail: describeReq(req) + " | " + strings.Join(append([]string{"回滚 ✓"}, problems...), " | ")})
		}
	}
	return results
}

// ---------------------------------------------------------------------------
// 并发(§5.4)
// ---------------------------------------------------------------------------

func (e *progEnv) concurrency() []Result {
	var results []Result
	hasCond, hasLock, hasIdem := -1, -1, -1
	for i, op := range e.ops {
		if op.Cond != nil && hasCond < 0 {
			hasCond = i
		}
		if op.Lock != nil && hasLock < 0 {
			hasLock = i
		}
		if op.Kind == "insert_idempotent" && hasIdem < 0 {
			hasIdem = i
		}
	}
	scenarios := 0
	if hasCond >= 0 {
		for rep := 1; rep <= 2; rep++ {
			results = append(results, e.burst(fmt.Sprintf("并发: 条件更新(%s)#%d", e.ops[hasCond].Name, rep), progAim{-1, command.OK}, true))
		}
		scenarios++
	}
	if hasLock >= 0 {
		for rep := 1; rep <= 2; rep++ {
			results = append(results, e.burst(fmt.Sprintf("并发: 同一个版本号(%s)#%d", e.ops[hasLock].Name, rep), progAim{-1, command.OK}, false))
		}
		results = append(results, e.lockLoop(hasLock))
		scenarios++
	}
	if hasIdem >= 0 {
		for rep := 1; rep <= 2; rep++ {
			results = append(results, e.burst(fmt.Sprintf("并发: 幂等新建(%s)#%d", e.ops[hasIdem].Name, rep), progAim{hasIdem, command.Created}, false))
		}
		scenarios++
	}
	if scenarios == 0 {
		results = append(results, e.burst("并发: 同一个请求", progAim{-1, command.OK}, false))
	}
	return results
}

// serial: 同一个请求在模型上串行执行 n 次: 每次的结果 + 第 s 次成功之后的状态(snaps[0] = 之前)。
func (e *progEnv) serial(req map[string]any, n int) ([]command.ProgResult, []*command.State) {
	m := e.state.Clone()
	snaps := []*command.State{m.Clone()}
	var rs []command.ProgResult
	for i := 0; i < n; i++ {
		r := m.Run(e.ops, e.inputs(req), e.now())
		rs = append(rs, r)
		if r.Outcome == command.OK {
			snaps = append(snaps, m.Clone())
		}
	}
	return rs, snaps
}

// burst: 同一个请求同时发 concurrentN 个。spread = 选串行成功次数在中间的请求(条件更新: 一部分成功、一部分不满足)。
func (e *progEnv) burst(name string, aim progAim, spread bool) Result {
	fail := func(f string, a ...any) Result {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: fmt.Sprintf(f, a...)}
	}
	e.seq++
	var req map[string]any
	var rs []command.ProgResult
	var snaps []*command.State
	best := -1
	for i := 0; i < 20; i++ {
		cand := e.find(aim)
		if cand == nil && req == nil && i == 10 { // 状态里没有合适的行了(前面的场景用完了): 新的随机初始状态
			if err := e.load(); err != nil {
				return fail("初始状态: %v", err)
			}
			cand = e.find(aim)
		}
		if cand == nil {
			continue
		}
		crs, csnaps := e.serial(cand, concurrentN)
		s := len(csnaps) - 1
		score := s
		if spread {
			score = -abs(2*s - concurrentN) // 越接近一半越好
			if s == 0 {
				score = -1000
			}
		}
		if req == nil || score > best {
			req, rs, snaps, best = cand, crs, csnaps, score
		}
		if !spread || (s >= 2 && s <= concurrentN-1) {
			break
		}
	}
	if req == nil {
		return fail("造不出这种请求")
	}
	sMax := len(snaps) - 1
	allowed := map[string]bool{"ABORTED": true} // 并发冲突重试之后仍然失败(序列化失败 / 死锁)
	for _, r := range rs {
		if r.Outcome != command.OK {
			allowed[r.Code()] = true
		}
	}
	replies := make([]reply, concurrentN)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range replies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			replies[i] = e.call(req, e.nowText(), 0)
		}(i)
	}
	close(start)
	wg.Wait()
	s, aborted, created := 0, 0, 0
	var problems []string
	var first map[string]any
	for _, r := range replies {
		switch {
		case r.err != nil:
			problems = append(problems, r.err.Error())
		case r.code == "OK":
			s++
			if e.createdOf(r.got) {
				created++
			}
			if first == nil {
				first = r.got
			} else if d := e.sameOutputs(first, r.got); d != "" {
				if aim.out == command.Created { // 幂等新建: 所有请求的结果相同(除了 新建)
					problems = append(problems, "幂等新建的结果不同: "+d)
				}
			}
		case !allowed[r.code]:
			problems = append(problems, fmt.Sprintf("错误码 %s(%s)不是串行执行时会出现的", r.code, r.msg))
		}
		if r.code == "ABORTED" {
			aborted++
		}
	}
	if s > sMax {
		problems = append(problems, fmt.Sprintf("成功 %d 次,超过理论上限 %d(串行执行的成功次数)", s, sMax))
	}
	if aborted == 0 && s < sMax {
		problems = append(problems, fmt.Sprintf("成功 %d 次,少于串行执行的 %d 次(没有并发冲突的错误,却有请求失败)", s, sMax))
	}
	if aim.out == command.Created && e.hasCreatedOutput() && created != 1 {
		problems = append(problems, fmt.Sprintf("新建 = true 的回答有 %d 个(应该正好 1 个)", created))
	}
	db, err := e.readState()
	if err != nil {
		return fail("%v", err)
	}
	if s <= sMax {
		if d := e.diffState(snaps[s], db, true); len(d) > 0 {
			problems = append(problems, fmt.Sprintf("状态 ≠ 串行执行 %d 次的状态: %s", s, strings.Join(d, "; ")))
			e.syncState(db)
		} else {
			e.state = snaps[s]
		}
	} else {
		e.syncState(db)
	}
	detail := fmt.Sprintf("%s × %d → 成功 %d / 理论上限 %d / ABORTED %d", describeReq(req), concurrentN, s, sMax, aborted)
	if aim.out == command.Created {
		detail += fmt.Sprintf(" / 新建 = true %d", created)
	}
	if len(problems) > 0 {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: detail + " | " + strings.Join(problems, " | ")}
	}
	return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: true, Detail: detail}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (e *progEnv) hasCreatedOutput() bool {
	for _, o := range e.outputs() {
		if o.Created {
			return true
		}
	}
	return false
}

func (e *progEnv) createdOf(got map[string]any) bool {
	for i, o := range e.outputs() {
		if o.Created {
			return fmt.Sprint(got[e.api.Response.Fields[i].Name]) == "true"
		}
	}
	return false
}

// sameOutputs: 两个回答的输出(除了 新建)相同吗。
func (e *progEnv) sameOutputs(a, b map[string]any) string {
	for i, o := range e.outputs() {
		if o.Created {
			continue
		}
		n := e.api.Response.Fields[i].Name
		if fmt.Sprint(a[n]) != fmt.Sprint(b[n]) {
			return fmt.Sprintf("%s: %v / %v", n, a[n], b[n])
		}
	}
	return ""
}

// lockLoop: 乐观锁的写入方的循环(读版本 → 写 → 版本冲突就重读),lockWorkers 个同时跑,各成功 lockSuccesses 次:
// 版本号 = 初始 + 成功的总数,状态 = 串行执行同样次数的状态(没有丢失的更新)。
func (e *progEnv) lockLoop(k int) Result {
	op := e.ops[k]
	name := fmt.Sprintf("并发: 乐观锁的重读循环(%s)", op.Name)
	fail := func(f string, a ...any) Result {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: fmt.Sprintf(f, a...)}
	}
	total := lockWorkers * lockSuccesses
	e.seq++
	// 请求: 全部成功,而且换成新的版本号重复执行也一直成功(条件被自己的效果变成假的命令不跑这个循环)
	var req map[string]any
	var keyRow command.Row
	for i := 0; i < 20 && req == nil; i++ {
		cand := e.find(progAim{-1, command.OK})
		if cand == nil {
			continue
		}
		m := e.state.Clone()
		ok := true
		for j := 0; j < total && ok; j++ {
			c := e.withVersion(cand, m, op)
			ok = c != nil && m.Run(e.ops, e.inputs(c), e.now()).Outcome == command.OK
		}
		if ok {
			req = cand
		}
	}
	if req == nil {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: true, Detail: "跳过: 重复执行会失败(条件被自己的效果变成假的),同一个版本号的并发已经检查"}
	}
	_ = keyRow
	var mu sync.Mutex
	succ, attempts := 0, 0
	var problems []string
	var wg sync.WaitGroup
	for w := 0; w < lockWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mine := 0
			for try := 0; try < 60 && mine < lockSuccesses; try++ {
				v, err := e.readVersion(req, op)
				if err != nil {
					mu.Lock()
					problems = append(problems, err.Error())
					mu.Unlock()
					return
				}
				c := map[string]any{}
				for kk, vv := range req {
					c[kk] = vv
				}
				c[op.Lock.Expr.Name] = e.jsonOf(e.api.Request.Field(op.Lock.Expr.Name), v)
				r := e.call(c, e.nowText(), 0)
				mu.Lock()
				attempts++
				switch r.code {
				case "OK":
					mine++
					succ++
				case "ABORTED":
				default:
					problems = append(problems, fmt.Sprintf("错误码 %s(%s)", r.code, r.msg))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	m := e.state.Clone()
	for j := 0; j < succ; j++ {
		c := e.withVersion(req, m, op)
		if c == nil || m.Run(e.ops, e.inputs(c), e.now()).Outcome != command.OK {
			return fail("模型: 第 %d 次串行执行失败", j+1)
		}
	}
	db, err := e.readState()
	if err != nil {
		return fail("%v", err)
	}
	if succ != total {
		problems = append(problems, fmt.Sprintf("成功 %d 次,期待 %d 次(重读之后应该能写进去)", succ, total))
	}
	if d := e.diffState(m, db, true); len(d) > 0 {
		problems = append(problems, "状态 ≠ 串行执行同样次数的状态(丢失的更新?): "+strings.Join(d, "; "))
		e.syncState(db)
	} else {
		e.state = m
	}
	detail := fmt.Sprintf("%d 个写入方 × %d 次: 成功 %d / 尝试 %d(其余是版本冲突后重读)", lockWorkers, lockSuccesses, succ, attempts)
	if len(problems) > 0 {
		return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: false, Detail: detail + " | " + strings.Join(problems, " | ")}
	}
	return Result{Spec: e.api.ID, Suite: "diff", Case: name, Passed: true, Detail: detail + ",版本号 = 初始 + 成功次数 ✓"}
}

// withVersion: 请求的版本号换成模型里这一行现在的版本号。
func (e *progEnv) withVersion(req map[string]any, m *command.State, op ir.CommandOp) map[string]any {
	key := command.Row{}
	for _, k := range op.Key {
		if k.Expr.Op != "input" {
			return nil
		}
		key[k.Column], _ = command.Canon(k.Type, req[k.Expr.Name])
	}
	row := findRow(m.Tables[op.Table], key)
	if row == nil {
		return nil
	}
	c := map[string]any{}
	for kk, vv := range req {
		c[kk] = vv
	}
	c[op.Lock.Expr.Name] = e.jsonOf(e.api.Request.Field(op.Lock.Expr.Name), rawOf(row[op.Lock.Column]))
	return c
}

// readVersion: 数据库里这一行现在的版本号(写入方读到的)。
func (e *progEnv) readVersion(req map[string]any, op ir.CommandOp) (int64, error) {
	var conds []string
	var args []any
	for _, k := range op.Key {
		args = append(args, req[k.Expr.Name])
		conds = append(conds, fmt.Sprintf("%s::text = $%d::text", pgx.Identifier{k.Column}.Sanitize(), len(args)))
	}
	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s", pgx.Identifier{op.Lock.Column}.Sanitize(), pgx.Identifier{op.Table}.Sanitize(), strings.Join(conds, " AND "))
	conn, err := pgx.Connect(e.ctx, e.conn.Config().ConnString())
	if err != nil {
		return 0, err
	}
	defer conn.Close(e.ctx)
	var v int64
	for i, a := range args {
		args[i] = fmt.Sprint(a)
	}
	return v, conn.QueryRow(e.ctx, q, args...).Scan(&v)
}

// ---------------------------------------------------------------------------
// 重放(§5.5)
// ---------------------------------------------------------------------------

func (e *progEnv) replay() []Result {
	idem := -1
	for i, op := range e.ops {
		if op.Kind != "insert_idempotent" {
			return nil // 不可重入的操作: 重放会再写一次(可重入的检查是 R4 的 DRC)
		}
		if idem < 0 {
			idem = i
		}
	}
	if idem < 0 {
		return nil
	}
	var results []Result
	for _, aim := range []progAim{{idem, command.Created}, {idem, command.Hit}} {
		req := e.find(aim)
		if req == nil {
			results = append(results, Result{Spec: e.api.ID, Suite: "diff", Case: "重放 " + aim.String(), Passed: false, Detail: "造不出这种请求"})
			continue
		}
		var first map[string]any
		for n := 1; n <= 3; n++ {
			name := fmt.Sprintf("重放: %s 第 %d 次", aim, n)
			res, want, r := e.step(name, req)
			if n > 1 && res.Passed {
				var problems []string
				if want.Outcome != command.OK || (len(want.Ops) > idem && want.Ops[idem].Created) {
					problems = append(problems, "模型: 重放应该命中已有的行")
				}
				if d := e.sameOutputs(first, r.got); d != "" {
					problems = append(problems, "结果和第一次不同: "+d)
				}
				if e.createdOf(r.got) {
					problems = append(problems, "重放的 新建 应该是 false")
				}
				if len(problems) > 0 {
					res.Passed, res.Detail = false, res.Detail+" | "+strings.Join(problems, " | ")
				} else {
					res.Detail += " | 和第一次相同 ✓"
				}
			}
			if n == 1 {
				first = r.got
			}
			results = append(results, res)
		}
	}
	return results
}
