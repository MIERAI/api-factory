package command

// 组合命令(R3-2)在模型状态上的执行: 按顺序的写操作,一个事务(任何一个失败 → 状态不变,返回那个操作的结果)。
// 条件更新、幂等新建、后面的操作引用前面的返回值都在这里重现 —— 和运行时(txn)、生成的代码各自独立实现,
// 状态差分测试比较两边。表达式的语义(factory/command/expr.go): 运算的任一边为空 → 空;比较为空 → 不成立。

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"apifactory/factory/ir"
)

// 组合命令另外的结果。
const (
	Unmet   Outcome = "条件不满足"  // 条件更新: FAILED_PRECONDITION + 业务原因码
	Check   Outcome = "数据库的规则" // 值超出列的范围(整数溢出)等: INVALID_ARGUMENT
	Created Outcome = "新建"     // 幂等新建: 真的插入了(覆盖条件用;响应是 OK)
	Hit     Outcome = "幂等命中"   // 幂等新建: 键已经有了,返回已有的行(覆盖条件用;响应是 OK)
)

// Inputs: 请求字段 → 值(数据库的写法: 枚举 = 数据库值;没有 = 请求里没有传)。
type Inputs map[string]any

// OpResult: 一个操作的结果。Row = 写之后的行(新建 / 更新)、删除的行、幂等命中时已有的行。
// 新建的行和状态里的是同一个 map: 状态采用数据库生成的值(Diff 的 adopt)之后,这里也看得到。
type OpResult struct {
	Ran     bool
	Row     Row
	Created bool
}

// ProgResult: 组合命令的结果。Failed = 失败的操作的下标(成功 = -1),Reason = 条件不满足时的业务原因码。
type ProgResult struct {
	Outcome Outcome
	Failed  int
	Reason  string
	Ops     []OpResult
	Detail  string
}

// ProgCode: 结果 → 错误码。
func (r ProgResult) Code() string {
	switch r.Outcome {
	case Unmet:
		return "FAILED_PRECONDITION"
	case Check:
		return "INVALID_ARGUMENT"
	}
	return r.Outcome.Code()
}

// Run: 在模型上执行组合命令。成功 → 状态改变;失败 → 状态不变。now = 请求开始的时刻(规范值)。
func (s *State) Run(ops []ir.CommandOp, in Inputs, now any) ProgResult {
	work := s.Clone()
	res := ProgResult{Failed: -1, Outcome: OK, Ops: make([]OpResult, len(ops))}
	refs := map[string]int{}
	for i, op := range ops {
		refs[op.Name] = i
	}
	run := func(i int, item Inputs) bool {
		op := ops[i]
		ts := work.Tables[op.Table]
		if ts == nil {
			res.Outcome, res.Failed, res.Detail = Invalid, i, "no table "+op.Table
			return false
		}
		r, outcome, detail := work.exec(ts, op, &evalCtx{in: in, item: item, now: now, prev: res.Ops[:i], refs: refs})
		if outcome != OK {
			res.Outcome, res.Failed, res.Detail = outcome, i, op.Name+": "+detail
			if outcome == Unmet {
				res.Reason = op.Unmet
			}
			res.Ops = res.Ops[:i]
			return false
		}
		res.Ops[i] = r
		return true
	}
	for i := 0; i < len(ops); {
		if ops[i].Each == "" {
			if !run(i, nil) {
				return res
			}
			i++
			continue
		}
		j := i // 每项(R5): 连续的同一个 每项 的操作 = 一个循环,每个元素按顺序执行一遍
		for j < len(ops) && ops[j].Each == ops[i].Each {
			j++
		}
		for _, it := range EachItems(in[ops[i].Each], ops[i].EachKey) {
			for k := i; k < j; k++ {
				if !run(k, it) {
					return res
				}
			}
		}
		i = j
	}
	s.Tables = work.Tables
	return res
}

// EachItems: 请求的列表字段 → 元素(按 keys 排序;没有 keys = 原来的顺序)。
func EachItems(v any, keys []string) []Inputs {
	var out []Inputs
	switch l := v.(type) {
	case []Inputs:
		out = append(out, l...)
	case []any:
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				out = append(out, Inputs(m))
			}
		}
	case []map[string]any:
		for _, m := range l {
			out = append(out, Inputs(m))
		}
	}
	if len(keys) > 0 {
		sort.SliceStable(out, func(i, j int) bool { return lessKey(out[i], out[j], keys) })
	}
	return out
}

type evalCtx struct {
	in   Inputs
	item Inputs // 每项: 当前元素(R4)
	env  string // 消息的 id(R4)
	// lenient: 引用没执行的操作(仅当)= 空(消息处理器)
	lenient bool
	now     any
	row     Row // update 的对象(更新之前)
	prev    []OpResult
	refs    map[string]int
	loads   map[string][]Inputs // 状态机: 载入的子行(R5)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// exec: 一个操作(状态 = 这个事务里到目前为止的状态)。
func (s *State) exec(ts *TableState, op ir.CommandOp, ec *evalCtx) (OpResult, Outcome, string) {
	switch op.Kind {
	case "insert", "insert_idempotent":
		row := Row{}
		for _, c := range ts.T.Cols {
			switch {
			case c.Default == nil:
				row[c.Name] = nil
			case c.Default.Generated:
				row[c.Name] = NewGen()
			default:
				row[c.Name] = c.Default.Value
			}
		}
		for _, v := range op.Values {
			if v.Expr.Op == "input" { // 可以不传的输入(新建): 不传 = 不写这一列(默认值)
				if _, ok := ec.in[v.Expr.Name]; !ok {
					continue
				}
			}
			x, err := ec.eval(v.Expr)
			if err != nil {
				return OpResult{}, Check, err.Error()
			}
			row[v.Column] = x
		}
		for _, c := range ts.T.Cols { // NOT NULL 在冲突的判定之前(Postgres 先组装行)
			if c.NotNull && row[c.Name] == nil {
				return OpResult{}, NotNull, c.Name
			}
		}
		if op.Kind == "insert_idempotent" {
			if j := conflictRow(ts, row, op.Conflict); j >= 0 {
				return OpResult{Ran: true, Row: ts.Rows[j], Created: false}, OK, ""
			}
		}
		if r, bad := s.violates(ts, row, -1); bad {
			return OpResult{}, r.Outcome, r.Detail
		}
		ts.Rows = append(ts.Rows, row)
		return OpResult{Ran: true, Row: row, Created: true}, OK, ""
	case "update", "delete":
		key := Row{}
		for _, k := range op.Key {
			x, err := ec.eval(k.Expr)
			if err != nil {
				return OpResult{}, Check, err.Error()
			}
			key[k.Column] = x
		}
		i := ts.find(key)
		if i < 0 {
			return OpResult{}, NotFound, ""
		}
		old := ts.Rows[i]
		ec.row = old
		if op.Lock != nil {
			v, err := ec.eval(op.Lock.Expr)
			if err != nil {
				return OpResult{}, Check, err.Error()
			}
			if v == nil || !Equal(old[op.Lock.Column], v) {
				return OpResult{}, Conflict, fmt.Sprintf("版本 %s,请求 %s", Show(old[op.Lock.Column]), Show(v))
			}
		}
		if op.Cond != nil {
			ok, err := ec.cond(op.Cond)
			if err != nil {
				return OpResult{}, Check, err.Error()
			}
			if !ok { // Row = 现在的行(R4: 不满足 = 跳过 时后面的操作 / 结果看它)
				return OpResult{Row: old}, Unmet, Format(op.Cond)
			}
		}
		if op.Kind == "delete" {
			for _, fk := range ts.T.RefBy {
				if child := s.Tables[fk.RefTable]; child != nil {
					for _, cr := range child.Rows {
						if refers(cr, fk.RefCols, old, fk.Cols) {
							return OpResult{}, ForeignKey, fk.Name
						}
					}
				}
			}
			ts.Rows = append(ts.Rows[:i:i], ts.Rows[i+1:]...)
			return OpResult{Ran: true, Row: old}, OK, ""
		}
		row := old.clone()
		for _, v := range op.Values { // SET 的表达式都按更新之前的行算
			x, err := ec.eval(v.Expr)
			if err != nil {
				return OpResult{}, Check, err.Error()
			}
			row[v.Column] = x
		}
		if op.Lock != nil {
			n, err := strconv.ParseInt(fmt.Sprint(old[op.Lock.Column]), 10, 64)
			if err != nil {
				return OpResult{}, Invalid, "version is not an integer: " + Show(old[op.Lock.Column])
			}
			row[op.Lock.Column] = strconv.FormatInt(n+1, 10)
		}
		if r, bad := s.violates(ts, row, i); bad {
			return OpResult{}, r.Outcome, r.Detail
		}
		ts.Rows[i] = row
		return OpResult{Ran: true, Row: row}, OK, ""
	}
	return OpResult{}, Invalid, "unknown op " + op.Kind
}

// conflictRow: 幂等键和 row 相同的已有的行(键有空值 → 不冲突)。
func conflictRow(ts *TableState, row Row, cols []string) int {
	for _, c := range cols {
		if row[c] == nil {
			return -1
		}
	}
	for j, r := range ts.Rows {
		if sameKey(row, r, cols) {
			return j
		}
	}
	return -1
}

// eval: 值的表达式 → 规范值。整数的运算按值的类型检查范围(超出 → 数据库的错误 22003)。
func (ec *evalCtx) eval(x *ir.CmdExpr) (any, error) {
	switch x.Op {
	case "input":
		v, ok := ec.in[x.Name]
		if !ok || v == nil {
			return nil, nil
		}
		return Canon(x.Type, v)
	case "const":
		return Canon(x.Type, x.Value)
	case "now":
		return ec.now, nil
	case "item":
		v, ok := ec.item[x.Name]
		if !ok || v == nil {
			return nil, nil
		}
		return Canon(x.Type, v)
	case "env":
		return ec.env, nil
	case "null":
		return nil, nil
	case "if":
		ok, err := ec.cond(x.Args[0])
		if err != nil {
			return nil, err
		}
		if ok {
			return ec.eval(x.Args[1])
		}
		return ec.eval(x.Args[2])
	case "col":
		if ec.row == nil {
			return nil, fmt.Errorf("column %s without a row", x.Name)
		}
		return ec.row[x.Name], nil
	case "ref":
		r, ok := ec.lookupRef(x.Ref)
		if !ok && ec.lenient { // 消息处理器: 仅当 没执行的操作 → 空
			return nil, nil
		}
		if !ok {
			return nil, fmt.Errorf("op %s has no result", x.Ref)
		}
		return r.Row[x.Name], nil
	case "+", "-", "*":
		a, err := ec.eval(x.Args[0])
		if err != nil {
			return nil, err
		}
		b, err := ec.eval(x.Args[1])
		if err != nil {
			return nil, err
		}
		if a == nil || b == nil {
			return nil, nil
		}
		if isGen(a) || isGen(b) {
			return Gen{}, nil
		}
		if x.Type == "numeric" {
			ra, _ := new(big.Rat).SetString(fmt.Sprint(a))
			rb, _ := new(big.Rat).SetString(fmt.Sprint(b))
			if ra == nil || rb == nil {
				return nil, fmt.Errorf("not a decimal: %v %v", a, b)
			}
			switch x.Op {
			case "+":
				return new(big.Rat).Add(ra, rb).RatString(), nil
			case "*":
				return new(big.Rat).Mul(ra, rb).RatString(), nil
			}
			return new(big.Rat).Sub(ra, rb).RatString(), nil
		}
		ia, err1 := strconv.ParseInt(fmt.Sprint(a), 10, 64)
		ib, err2 := strconv.ParseInt(fmt.Sprint(b), 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("not an integer: %v %v", a, b)
		}
		ba, bb := big.NewInt(ia), big.NewInt(ib)
		var n *big.Int
		switch x.Op {
		case "+":
			n = new(big.Int).Add(ba, bb)
		case "*":
			n = new(big.Int).Mul(ba, bb)
		default:
			n = new(big.Int).Sub(ba, bb)
		}
		lo, hi := intRange(x.Type)
		if n.Cmp(big.NewInt(lo)) < 0 || n.Cmp(big.NewInt(hi)) > 0 {
			return nil, fmt.Errorf("%s out of range for %s", n, x.Type)
		}
		return n.String(), nil
	}
	return nil, fmt.Errorf("%s is not a value", x.Op)
}

func (ec *evalCtx) lookupRef(name string) (OpResult, bool) {
	i, ok := ec.refs[name]
	if !ok || i >= len(ec.prev) || (!ec.prev[i].Ran && ec.prev[i].Row == nil) { // 跳过的更新: Row = 现在的行
		return OpResult{}, false
	}
	return ec.prev[i], true
}

// cond: 条件成立吗(空 → 不成立)。
func (ec *evalCtx) cond(x *ir.CmdExpr) (bool, error) {
	if x.Op == "and" {
		for _, a := range x.Args {
			ok, err := ec.cond(a)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	}
	a, err := ec.eval(x.Args[0])
	if err != nil {
		return false, err
	}
	b, err := ec.eval(x.Args[1])
	if err != nil {
		return false, err
	}
	if a == nil || b == nil {
		return false, nil
	}
	c, err := compareCanon(typeOfCmp(x), a, b)
	if err != nil {
		return false, err
	}
	switch x.Op {
	case "=":
		return c == 0, nil
	case "<>":
		return c != 0, nil
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	case ">=":
		return c >= 0, nil
	}
	return false, fmt.Errorf("unknown comparison %s", x.Op)
}

// typeOfCmp: 比较的类型(两边较宽的一个)。
func typeOfCmp(x *ir.CmdExpr) string {
	a, b := x.Args[0].Type, x.Args[1].Type
	if a == "numeric" || b == "numeric" {
		return "numeric"
	}
	if isInt(a) || isInt(b) {
		return "int8"
	}
	if a != "" {
		return a
	}
	return b
}

func compareCanon(typ string, a, b any) (int, error) {
	switch {
	case typ == "numeric" || isInt(typ):
		ra, ok1 := new(big.Rat).SetString(fmt.Sprint(a))
		rb, ok2 := new(big.Rat).SetString(fmt.Sprint(b))
		if !ok1 || !ok2 {
			return 0, fmt.Errorf("not a number: %v %v", a, b)
		}
		return ra.Cmp(rb), nil
	case typ == "timestamptz" || typ == "timestamp" || typ == "date": // 规范形(UTC、固定的格式)按文本比较 = 按时间比较
		return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)), nil
	}
	if a == b {
		return 0, nil
	}
	return 1, nil // 文本 / 布尔: 只有 = / <>
}

func intRange(t string) (int64, int64) {
	switch t {
	case "int2":
		return -32768, 32767
	case "int4":
		return -2147483648, 2147483647
	}
	return -9223372036854775808, 9223372036854775807
}

func isGen(v any) bool { _, ok := v.(Gen); return ok }

// OpsOf: 命令 → 操作的列表(组合命令原样;新建 / 按键更新 / 按键删除 = 一个操作)。
// 故障注入 / 并发 / 重放的测试对所有的命令用同一套模型。
func OpsOf(c *ir.Command) []ir.CommandOp {
	if c.Op == "command" || c.Op == "message" {
		return c.Ops
	}
	op := ir.CommandOp{Name: c.Op, Kind: c.Op, Table: c.Table, NotFound: c.NotFound, Returning: c.Returning}
	in := func(b ir.CommandCol) ir.CommandVal {
		return ir.CommandVal{Column: b.Column, Type: b.Type, NotNull: b.NotNull, Expr: &ir.CmdExpr{Op: "input", Name: b.Field, Type: b.Type}}
	}
	for _, b := range c.Key {
		op.Key = append(op.Key, in(b))
	}
	for _, b := range c.Write {
		op.Values = append(op.Values, in(b))
	}
	for _, s := range c.Set {
		v := ir.CommandVal{Column: s.Column, Type: s.Type, Expr: &ir.CmdExpr{Op: "const", Value: s.Value, Type: s.Type}}
		if s.Now {
			v.Expr = &ir.CmdExpr{Op: "now", Type: "timestamptz"}
		}
		op.Values = append(op.Values, v)
	}
	if c.Lock != nil {
		v := in(*c.Lock)
		op.Lock = &v
	}
	return []ir.CommandOp{op}
}
