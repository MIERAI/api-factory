// Package core 是工厂的核心代数: 基本元件(封闭集合)的定义。
//
// 设计稿: docs/design/core-algebra.md
//
// 只有这里定义的元件有实现,而且只有两份:
//   - SQL 后端(sql.go): 给服务器
//   - 参考求值器(eval.go): 给测试,在内存里逐行计算,独立于 SQL
//
// 单元库(属于、有效期包含、多条……)只是这些元件的组合,不带实现。
// 新增元件要单独评审,并且必须通过元件认证(cert_test.go)。
package core

import (
	"fmt"
	"math/big"
	"time"
)

// ---------------------------------------------------------------------------
// 类型
// ---------------------------------------------------------------------------

type Kind string

const (
	Int32     Kind = "int32"
	Int64     Kind = "int64"
	Text      Kind = "text"
	Bool      Kind = "bool"
	Decimal   Kind = "decimal"   // 精确的十进制(SQL numeric;参考实现 big.Rat;API 里是十进制字符串)
	Date      Kind = "date"      // 日期(API 里 YYYY-MM-DD)
	Timestamp Kind = "timestamp" // 时刻(API 里 RFC3339 UTC)
	JSON      Kind = "json"      // 只能作为 json_field 的输入
	// Rows: 子行的列表(nest 加的列)。Type.Fields = 每个子行的列。不能比较、排序、分组;值是 Children
	Rows Kind = "rows"
)

type Type struct {
	Kind     Kind        `json:"kind"`
	Nullable bool        `json:"nullable,omitempty"`
	Fields   []NamedType `json:"fields,omitempty"` // Kind = rows: 子行的列(按顺序)
}

// SQLType: SQL 里的类型名(参数的 cast 用)。
func (k Kind) SQLType() string {
	switch k {
	case Int32:
		return "int4"
	case Int64:
		return "int8"
	case Text:
		return "text"
	case Bool:
		return "bool"
	case Decimal:
		return "numeric"
	case Date:
		return "date"
	case Timestamp:
		return "timestamptz"
	case JSON, Rows:
		return "jsonb"
	}
	panic(fmt.Sprintf("core: unknown kind %q", k))
}

func comparable(a, b Kind) bool {
	if a == b {
		return a != JSON && a != Rows
	}
	return (a == Int32 || a == Int64) && (b == Int32 || b == Int64)
}

// ---------------------------------------------------------------------------
// 表达式元件
// ---------------------------------------------------------------------------

// Expr 的 Op:
//
//	col          Name = 列名
//	const        Value(Type 决定种类;nil = NULL)
//	param        Name = 参数名
//	cmp          Name ∈ = <> < <= > >=;Args[0], Args[1]。任一边 NULL → UNKNOWN
//	and / or     Args(≥ 2),Kleene 三值逻辑
//	not          Args[0]
//	is_null      Args[0];永远 TRUE / FALSE
//	absent       Name = 参数名: 标量没有指定 / 列表为空或没有指定。"不传"只在这里定义
//	in_list      Args[0] 属于参数 Name 的列表。Args[0] 为 NULL → UNKNOWN;空列表 → FALSE
//	starts_with  Args[0] 以参数 Name 开头(按字面,% _ \ 不是通配符);任一边 NULL → UNKNOWN
//	json_field   Args[0](json)里键 Name 的值转换成 Type。键不存在 / JSON null → NULL;格式错 → 查询失败
//	tuple_after  Args(列)按 Desc 方向严格在参数 Params 之后(keyset 游标)。参数 Params[0] 没有指定 → TRUE
//	in_tuples    组的列表: Params 是同样长的列表参数(第 i 个组 = 每个参数的第 i 个值)。
//	             有一个组和 Args(列)逐个相等 → TRUE,否则 FALSE(两值,和 EXISTS 一样: 列为 NULL 时不相等,不是 UNKNOWN);空列表 → FALSE
type Expr struct {
	Op     string   `json:"op"`
	Name   string   `json:"name,omitempty"`
	Value  any      `json:"value,omitempty"`
	Type   *Type    `json:"type,omitempty"`
	Args   []*Expr  `json:"args,omitempty"`
	Params []string `json:"params,omitempty"`
	Desc   bool     `json:"desc,omitempty"`
}

func Col(name string) *Expr              { return &Expr{Op: "col", Name: name} }
func Param(name string) *Expr            { return &Expr{Op: "param", Name: name} }
func Const(v any, t Type) *Expr          { return &Expr{Op: "const", Value: v, Type: &t} }
func Cmp(op string, a, b *Expr) *Expr    { return &Expr{Op: "cmp", Name: op, Args: []*Expr{a, b}} }
func And(xs ...*Expr) *Expr              { return &Expr{Op: "and", Args: xs} }
func Or(xs ...*Expr) *Expr               { return &Expr{Op: "or", Args: xs} }
func Not(x *Expr) *Expr                  { return &Expr{Op: "not", Args: []*Expr{x}} }
func IsNull(x *Expr) *Expr               { return &Expr{Op: "is_null", Args: []*Expr{x}} }
func Absent(param string) *Expr          { return &Expr{Op: "absent", Name: param} }
func InList(x *Expr, param string) *Expr { return &Expr{Op: "in_list", Name: param, Args: []*Expr{x}} }
func StartsWith(x *Expr, param string) *Expr {
	return &Expr{Op: "starts_with", Name: param, Args: []*Expr{x}}
}
func JSONField(x *Expr, key string, t Type) *Expr {
	return &Expr{Op: "json_field", Name: key, Type: &t, Args: []*Expr{x}}
}
func TupleAfter(cols []string, params []string, desc bool) *Expr {
	e := &Expr{Op: "tuple_after", Params: params, Desc: desc}
	for _, c := range cols {
		e.Args = append(e.Args, Col(c))
	}
	return e
}

func InTuples(cols []string, params []string) *Expr {
	e := &Expr{Op: "in_tuples", Params: params}
	for _, c := range cols {
		e.Args = append(e.Args, Col(c))
	}
	return e
}

var cmpOps = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

// ---------------------------------------------------------------------------
// 关系元件(流水线的一步)
// ---------------------------------------------------------------------------

// Step 的 Kind:
//
//	scan          Table;Columns = 物理表的列(名字 → 类型)
//	filter        Pred: 只保留 TRUE 的行(UNKNOWN 丢弃)
//	project       Project: 新的列(名字 → 表达式),只保留这些列
//	pick_per_key  Keys: 每个键(NULL 视为相等)按 Order 取第一行
//	sort          Order: 文本按字节;NULL 在 ASC 时最后、DESC 时最前
//	limit         N;或者 Param = 参数名(必填的整数参数,例如分页的 row_limit)
//	lookup        多对一查找: 对每一行,在 Table(列 = Columns)里找 On 的键列逐个等于当前行的列的那一行,
//	              把 Take 的列加到当前行上。On 的键列必须覆盖 Table 的一个唯一键(Unique)→ 最多一行匹配。
//	              任一边为 NULL → 不匹配(和 SQL 的 = 一致)。找不到: Missing = keep(默认)→ 加的列为 NULL(类型可空);
//	              drop → 丢弃这一行(内连接)。后面的 lookup 可以用前面 lookup 加的列做关联(链式)
//	nest          一对多嵌套: 对每一行,取 Table(列 = Columns)里 On 的列逐个相等的行(任一边 NULL → 不相等),
//	              经过 Pred(子表的列上的条件,UNKNOWN 丢弃)、按 Order 排序,取 Take 的列,作为子行的列表加一列 As
//	              (类型 rows,值 Children)。没有子行 → 空列表(不是 NULL)。子行多于 N → 整个请求 FAILED_PRECONDITION
//	              (不截断;两个后端都最多取 N+1 行,超过由出口判定: 只看响应里的行)。
//	              DRC: Order + On 的子表的列覆盖子表的一个唯一键(不为空的列)→ 子行的顺序是全序。只有一层
//	child_agg     按父行聚合: 对每一行,对关联的子行(同 nest 的 On / Pred)计算 Aggs(agg.go,集合可能为空:
//	              count 0、sum 0、min / max NULL),作为标量列加到当前行上
type Step struct {
	Kind    string      `json:"kind"`
	Table   string      `json:"table,omitempty"`
	Columns []NamedType `json:"columns,omitempty"`
	Pred    *Expr       `json:"pred,omitempty"`
	Project []NamedExpr `json:"project,omitempty"`
	Keys    []string    `json:"keys,omitempty"`
	Order   []SortKey   `json:"order,omitempty"`
	N       int         `json:"n,omitempty"`
	Param   string      `json:"param,omitempty"` // limit: 行数来自参数
	From    string      `json:"from,omitempty"`  // 来自哪个单元 / 设计稿哪一行(审查和报错用)
	// lookup(Table / Columns 和 scan 一样是对方的表和列)
	On      []JoinKey  `json:"on,omitempty"`      // 当前行的列 = 对方的键列
	Take    []TakeCol  `json:"take,omitempty"`    // 加到当前行上的列(新列名 ← 对方的列)
	Missing string     `json:"missing,omitempty"` // keep(空 = keep)/ drop
	Unique  [][]string `json:"unique,omitempty"`  // 对方的表的唯一键(主键 / 唯一约束;DRC 用)
	// NullEq: 只给差分测试的"错误实现"用(NULL 当作相等的关联),参考求值器认,SQL 后端和 IR 里没有
	NullEq bool `json:"-"`
	// nest / child_agg(Table / Columns / On / Unique 同 lookup;On 的 Key = 子表的列。Pred = 子表的列上的条件;
	// Order = 子行的顺序;N = 子行的上限;Take = 子行的列)
	Aggs []Agg `json:"aggs,omitempty"` // child_agg: 聚合(agg.go)
	As   string `json:"as,omitempty"`   // nest: 子行的列表的列名
	// Mutant: 只给差分测试的"错误实现"用(参考求值器认,SQL 后端和 IR 里没有):
	// nest: empty_null(空列表 → NULL);child_agg: sum_empty_null(空集合的 sum → NULL)/ count_outer(空集合的 count = 1,
	// LEFT JOIN + GROUP BY 的 count(*))
	Mutant string `json:"-"`
}

// JoinKey: 关联条件的一对列(列 = 列的等值)。
type JoinKey struct {
	Col string `json:"col"` // 当前行的列
	Key string `json:"key"` // 对方的键列
}

// TakeCol: lookup 加到当前行上的一列。
type TakeCol struct {
	Name string `json:"name"` // 新列名
	Col  string `json:"col"`  // 对方的列
}

type NamedType struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
}

type NamedExpr struct {
	Name string `json:"name"`
	Expr *Expr  `json:"expr"`
}

type SortKey struct {
	Col  string `json:"col"`
	Desc bool   `json:"desc,omitempty"`
}

func Scan(table string, cols ...NamedType) Step {
	return Step{Kind: "scan", Table: table, Columns: cols}
}
func Filter(p *Expr) Step          { return Step{Kind: "filter", Pred: p} }
func Project(xs ...NamedExpr) Step { return Step{Kind: "project", Project: xs} }
func PickPerKey(keys []string, order ...SortKey) Step {
	return Step{Kind: "pick_per_key", Keys: keys, Order: order}
}
func Sort(order ...SortKey) Step { return Step{Kind: "sort", Order: order} }
func Limit(n int) Step           { return Step{Kind: "limit", N: n} }
func LimitParam(p string) Step   { return Step{Kind: "limit", Param: p} }

// Lookup: 多对一查找(missing: keep / drop)。
func Lookup(table string, cols []NamedType, unique [][]string, on []JoinKey, take []TakeCol, missing string) Step {
	return Step{Kind: "lookup", Table: table, Columns: cols, Unique: unique, On: on, Take: take, Missing: missing}
}

// Nest: 一对多嵌套(子行的列表加一列 as)。
func Nest(table string, cols []NamedType, unique [][]string, on []JoinKey, pred *Expr, order []SortKey, max int, as string, take []TakeCol) Step {
	return Step{Kind: "nest", Table: table, Columns: cols, Unique: unique, On: on, Pred: pred, Order: order, N: max, As: as, Take: take}
}

// ChildAgg: 按父行聚合(每个聚合加一列)。
func ChildAgg(table string, cols []NamedType, on []JoinKey, pred *Expr, aggs ...Agg) Step {
	return Step{Kind: "child_agg", Table: table, Columns: cols, On: on, Pred: pred, Aggs: aggs}
}

// Children: rows 类型的值(nest 的子行的列表)。Rows 最多 Max+1 行(两个后端都多取一行);Max < 0 = 不知道上限
// (从 Postgres 读回的值)。比较(Canonical)只看 Rows。
type Children struct {
	Rows []Row
	Max  int
}

// Over: 子行多于上限(整个请求 FAILED_PRECONDITION)。
func (c Children) Over() bool { return c.Max >= 0 && len(c.Rows) > c.Max }

// Overflow: 这一行里有没有超过上限的子行的列表(出口判定 FAILED_PRECONDITION 用)。
func Overflow(r Row) bool {
	for _, v := range r {
		if c, ok := v.(Children); ok && c.Over() {
			return true
		}
	}
	return false
}

// Pipeline: 一条查询。Params = 请求参数的类型(列表参数 List = true)。
type Pipeline struct {
	Steps  []Step      `json:"steps"`
	Params []ParamDecl `json:"params"`
}

type ParamDecl struct {
	Name     string `json:"name"`
	Type     Type   `json:"type"` // 元素类型
	List     bool   `json:"list,omitempty"`
	Required bool   `json:"required,omitempty"` // 必填: absent 恒为 FALSE
}

func (p *Pipeline) Param(name string) *ParamDecl {
	for i := range p.Params {
		if p.Params[i].Name == name {
			return &p.Params[i]
		}
	}
	return nil
}

// Normalize: 流水线从 JSON(IR 文件)读回来后,按声明的类型还原常量的值
// (JSON 里整数会变成 float64,decimal / 时间会变成字符串)。
func (p *Pipeline) Normalize() error {
	var walk func(e *Expr) error
	walk = func(e *Expr) error {
		if e == nil {
			return nil
		}
		if e.Op == "const" && e.Value != nil && e.Type != nil {
			v, err := normalizeConst(e.Value, e.Type.Kind)
			if err != nil {
				return err
			}
			e.Value = v
		}
		for _, a := range e.Args {
			if err := walk(a); err != nil {
				return err
			}
		}
		return nil
	}
	for i := range p.Steps {
		if err := walk(p.Steps[i].Pred); err != nil {
			return err
		}
		for _, x := range p.Steps[i].Project {
			if err := walk(x.Expr); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeConst(v any, k Kind) (any, error) {
	switch x := v.(type) {
	case float64:
		if k == Int32 || k == Int64 {
			return int64(x), nil
		}
		return castText(fmt.Sprint(x), k)
	case string:
		if k == Text {
			return x, nil
		}
		if k == Decimal {
			if r, ok := new(big.Rat).SetString(x); ok { // big.Rat 的 JSON 是 "3/2"
				return r, nil
			}
		}
		if k == Timestamp {
			return castText(x, k)
		}
		if k == Date {
			if t, err := time.Parse(time.RFC3339, x); err == nil {
				return t, nil
			}
		}
		return castText(x, k)
	}
	return v, nil
}
