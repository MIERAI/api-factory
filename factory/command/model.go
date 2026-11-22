package command

// 模型状态上的写操作(设计稿 §3: insert / update / delete 的语义)。和运行时(api-factory-runtime/txn)、
// 生成的代码各自独立实现;状态差分测试比较两边。

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"apifactory/factory/exits"
)

// Row: 一行(列 → 规范值)。
type Row map[string]any

func (r Row) clone() Row {
	out := make(Row, len(r))
	for k, v := range r {
		out[k] = v
	}
	return out
}

// TableState: 一张表的全部行。
type TableState struct {
	T    *Table
	Rows []Row
}

// State: 模型状态(命令写的表 + 外键相关的表)。
type State struct {
	Tables map[string]*TableState
}

func (s *State) Clone() *State {
	out := &State{Tables: map[string]*TableState{}}
	for n, t := range s.Tables {
		ts := &TableState{T: t.T, Rows: make([]Row, len(t.Rows))}
		for i, r := range t.Rows {
			ts.Rows[i] = r.clone()
		}
		out.Tables[n] = ts
	}
	return out
}

// Outcome: 写操作的结果(设计稿 §3.2 / §3.3 的分类)。
type Outcome string

const (
	OK         Outcome = "成功"
	NotFound   Outcome = "不存在"
	Conflict   Outcome = "版本冲突"
	Unique     Outcome = "唯一约束冲突"
	NotNull    Outcome = "NOT NULL"
	ForeignKey Outcome = "外键"
	Invalid    Outcome = "请求不合法" // protovalidate(请求的校验,在写操作之前)
)

// Code: 结果 → 错误码(设计稿的语义;⚖️ D1 的版本冲突用 exits.ConflictCode)。
func (o Outcome) Code() string {
	switch o {
	case OK:
		return "OK"
	case NotFound:
		return "NOT_FOUND"
	case Conflict:
		return exits.ConflictCode
	case Unique:
		return "ALREADY_EXISTS"
	case NotNull, Invalid, Check:
		return "INVALID_ARGUMENT"
	case ForeignKey:
		return "FAILED_PRECONDITION"
	}
	return "UNKNOWN"
}

// Request: 一个写操作的输入(列 → 规范值)。Write 里没有的列 = 请求里没有传(新建: 数据库的默认值)。
type Request struct {
	Key   Row
	Write Row
	Set   Row
	Lock  string // 乐观锁的版本列("" = 没有)
	LockV any    // 请求带的版本号
}

// Result: 写操作的结果。Row = 写之后的行(update / insert)或删除的行(delete)。
type Result struct {
	Outcome Outcome
	Row     Row
	Detail  string
}

// Apply: 在模型上执行一个写操作。成功时修改状态;失败时状态不变(一个事务)。
func (s *State) Apply(op, table string, req Request) Result {
	ts := s.Tables[table]
	if ts == nil {
		return Result{Outcome: Invalid, Detail: "no table " + table}
	}
	switch op {
	case "insert":
		row := Row{}
		for _, c := range ts.T.Cols {
			switch {
			case c.Default == nil:
				row[c.Name] = nil
			case c.Default.Generated:
				row[c.Name] = Gen{}
			default:
				row[c.Name] = c.Default.Value
			}
		}
		for k, v := range req.Write {
			row[k] = v
		}
		for k, v := range req.Set {
			row[k] = v
		}
		if r, bad := s.violates(ts, row, -1); bad {
			return r
		}
		ts.Rows = append(ts.Rows, row)
		return Result{Outcome: OK, Row: row.clone()}
	case "update", "delete":
		i := ts.find(req.Key)
		if i < 0 {
			return Result{Outcome: NotFound}
		}
		old := ts.Rows[i]
		if req.Lock != "" && !Equal(old[req.Lock], req.LockV) {
			return Result{Outcome: Conflict, Detail: fmt.Sprintf("版本 %s,请求 %s", Show(old[req.Lock]), Show(req.LockV))}
		}
		if op == "delete" {
			for _, fk := range ts.T.RefBy { // 还被引用(没有 ON DELETE 的动作)
				if child := s.Tables[fk.RefTable]; child != nil {
					for _, cr := range child.Rows {
						if refers(cr, fk.RefCols, old, fk.Cols) {
							return Result{Outcome: ForeignKey, Detail: fk.Name}
						}
					}
				}
			}
			ts.Rows = append(ts.Rows[:i:i], ts.Rows[i+1:]...)
			return Result{Outcome: OK, Row: old.clone()}
		}
		row := old.clone()
		for k, v := range req.Write {
			row[k] = v
		}
		for k, v := range req.Set {
			row[k] = v
		}
		if req.Lock != "" { // 版本 + 1
			n, err := strconv.ParseInt(fmt.Sprint(old[req.Lock]), 10, 64)
			if err != nil {
				return Result{Outcome: Invalid, Detail: "version is not an integer: " + Show(old[req.Lock])}
			}
			row[req.Lock] = strconv.FormatInt(n+1, 10)
		}
		if r, bad := s.violates(ts, row, i); bad {
			return r
		}
		ts.Rows[i] = row
		return Result{Outcome: OK, Row: row.clone()}
	}
	return Result{Outcome: Invalid, Detail: "unknown op " + op}
}

// violates: 新的行(self = 更新的那一行的下标,新建 = -1)违反 NOT NULL / 唯一约束 / 外键吗。
// 顺序和 Postgres 一样: NOT NULL → 唯一 → 外键。
func (s *State) violates(ts *TableState, row Row, self int) (Result, bool) {
	for _, c := range ts.T.Cols {
		if c.NotNull && row[c.Name] == nil {
			return Result{Outcome: NotNull, Detail: c.Name}, true
		}
	}
	for _, b := range ts.T.Bounds { // CHECK (列 比较 整数): 空 = 通过(SQL 的 CHECK)
		v, err := strconv.ParseInt(fmt.Sprint(row[b.Col]), 10, 64)
		if row[b.Col] == nil || isGen(row[b.Col]) || err != nil {
			continue
		}
		if ok := map[string]bool{">=": v >= b.N, ">": v > b.N, "<=": v <= b.N, "<": v < b.N}[b.Op]; !ok {
			return Result{Outcome: Check, Detail: b.Name}, true
		}
	}
	for _, u := range ts.T.Uniques {
		for j, other := range ts.Rows {
			if j != self && sameKey(row, other, u) {
				return Result{Outcome: Unique, Detail: strings.Join(u, ",")}, true
			}
		}
	}
	for _, fk := range ts.T.FKs {
		vals := make([]any, len(fk.Cols))
		null := false
		for i, c := range fk.Cols {
			vals[i] = row[c]
			null = null || row[c] == nil
		}
		if null { // MATCH SIMPLE: 有一列为空 → 不检查
			continue
		}
		parent := s.Tables[fk.RefTable]
		found := false
		if parent != nil {
			for _, pr := range parent.Rows {
				if refers(row, fk.Cols, pr, fk.RefCols) {
					found = true
					break
				}
			}
		}
		if !found {
			return Result{Outcome: ForeignKey, Detail: fk.Name}, true
		}
	}
	return Result{}, false
}

// sameKey: 两行在这些列上都不为空且相等(唯一约束: NULL 不相等)。生成值 → 不知道,不算冲突。
func sameKey(a, b Row, cols []string) bool {
	for _, c := range cols {
		x, y := a[c], b[c]
		if x == nil || y == nil {
			return false
		}
		if _, g := x.(Gen); g {
			return false
		}
		if _, g := y.(Gen); g {
			return false
		}
		if x != y {
			return false
		}
	}
	return true
}

func refers(child Row, ccols []string, parent Row, pcols []string) bool {
	for i := range ccols {
		if child[ccols[i]] == nil || !Equal(child[ccols[i]], parent[pcols[i]]) {
			return false
		}
	}
	return true
}

// find: 主键 = key 的行的下标(没有 → -1)。
func (ts *TableState) find(key Row) int {
	for i, r := range ts.Rows {
		ok := true
		for c, v := range key {
			if !Equal(r[c], v) || r[c] == nil {
				ok = false
				break
			}
		}
		if ok && len(key) > 0 {
			return i
		}
	}
	return -1
}

// KeyOf: 一行的主键(文本,排序 / 对应用)。
func (t *Table) KeyOf(r Row) string {
	var parts []string
	for _, c := range t.PK {
		parts = append(parts, Show(r[c]))
	}
	return strings.Join(parts, "|")
}

// genKey: 主键里有数据库生成的值(新建的行,序号还不知道)。
func (t *Table) genKey(r Row) bool {
	for _, c := range t.PK {
		if _, g := r[c].(Gen); g {
			return true
		}
	}
	return false
}

// keyLess: 主键的顺序(整数按数值;序号 = 插入的顺序)。
func (t *Table) keyLess(a, b Row) bool {
	for _, c := range t.PK {
		x, y := fmt.Sprint(a[c]), fmt.Sprint(b[c])
		if x == y {
			continue
		}
		nx, ex := strconv.ParseInt(x, 10, 64)
		ny, ey := strconv.ParseInt(y, 10, 64)
		if ex == nil && ey == nil {
			return nx < ny
		}
		return x < y
	}
	return false
}

// Diff: 两份状态(模型 vs 数据库)的差别。按主键对应,比较全部的列;模型的生成值和数据库的任何不为空的值相等。
// 返回差别的说明(一致 → "")。adopt = true 时,模型的生成值换成数据库的值。
func Diff(t *Table, model, db []Row, adopt bool) string {
	byKey := map[string]Row{}
	for _, r := range db {
		byKey[t.KeyOf(r)] = r
	}
	var out []string
	seen := map[string]bool{}
	compare := func(k string, m, d Row) {
		for _, c := range t.Cols {
			if !Equal(m[c.Name], d[c.Name]) {
				out = append(out, fmt.Sprintf("%s[%s].%s: 模型 %s,数据库 %s", t.Name, k, c.Name, Show(m[c.Name]), Show(d[c.Name])))
			} else if _, g := m[c.Name].(Gen); g && adopt {
				m[c.Name] = d[c.Name]
			}
		}
	}
	var pending []Row // 主键是数据库生成的(序号)新的行: 和数据库里剩下的行按序号的顺序对应
	for _, m := range model {
		if t.genKey(m) {
			pending = append(pending, m)
			continue
		}
		k := t.KeyOf(m)
		seen[k] = true
		d, ok := byKey[k]
		if !ok {
			out = append(out, fmt.Sprintf("%s[%s]: 模型有、数据库没有", t.Name, k))
			continue
		}
		compare(k, m, d)
	}
	var rest []Row
	for _, d := range db {
		if k := t.KeyOf(d); !seen[k] {
			rest = append(rest, d)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return t.keyLess(rest[i], rest[j]) })
	for i, m := range pending {
		if i >= len(rest) {
			out = append(out, fmt.Sprintf("%s[%s]: 模型有(新的行)、数据库没有", t.Name, t.KeyOf(m)))
			continue
		}
		compare(t.KeyOf(rest[i]), m, rest[i])
	}
	for _, d := range rest[min(len(pending), len(rest)):] {
		out = append(out, fmt.Sprintf("%s[%s]: 数据库有、模型没有", t.Name, t.KeyOf(d)))
	}
	sort.Strings(out)
	if len(out) > 4 {
		out = append(out[:4], fmt.Sprintf("… 共 %d 处", len(out)))
	}
	return strings.Join(out, "; ")
}
