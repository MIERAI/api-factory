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
//	contains     Args[0] 包含参数 Name(任何位置;按字面,% _ \ 不是通配符;空字符串 → TRUE);任一边 NULL → UNKNOWN。
//	             按字节比较(和 starts_with 一样)。btree 索引用不上(前面有通配符),大表要配 pg_trgm 的 GIN 索引
//	json_field   Args[0](json)里键 Name 的值转换成 Type。键不存在 / JSON null → NULL;格式错 → 查询失败
//	tuple_after  Args(列)按 Desc 方向严格在参数 Params 之后(keyset 游标)。参数 Params[0] 没有指定 → TRUE
//	in_tuples    组的列表: Params 是同样长的列表参数(第 i 个组 = 每个参数的第 i 个值)。
//	             有一个组和 Args(列)逐个相等 → TRUE,否则 FALSE(两值,和 EXISTS 一样: 列为 NULL 时不相等,不是 UNKNOWN);空列表 → FALSE
//	now          请求开始时的时刻(timestamp,不为 NULL)。值不来自请求: 服务器的时钟(测试时固定,见 NowParam)
//	add          Args[0](日期 / 时间)加 Args[1](整数)个单位,Name = 单位 ∈ minute hour day month;Neg = 减。
//	             月按 Postgres: 1 月 31 日 + 1 个月 = 2 月 28 日(29 日);日期只能加 day / month(结果还是日期)。
//	             时间的 day / month 按领域的时区(Zone)的日历加(minute / hour 是绝对时长)。任一边 NULL → NULL
//	trunc        Args[0] 截到 Name ∈ day month 的开始。时间先换算到领域的时区(Zone)再截,结果是那个时刻;
//	             日期的结果还是日期。NULL → NULL
//	ymd          Args[0](整数)按 YYYYMMDD 编码读成日期(旧的表结构常见: 20260131 = 2026-01-31)。
//	             不是合法的日期(20260230、20261301、0、负数、1000 年之前)→ NULL(比较是 UNKNOWN: 永远不命中)。NULL → NULL。
//	             SQL 后端: 在条件的肯定位置(filter 里,不在 not 之下)的 cmp(op, ymd(x), v) 写成 x op <v 的 YYYYMMDD 整数>
//	             AND <x 是合法的日期>(按列本来的编码比较: 整数列上的索引能用);其他位置按 CASE 换算
type Expr struct {
	Op     string   `json:"op"`
	Name   string   `json:"name,omitempty"`
	Value  any      `json:"value,omitempty"`
	Type   *Type    `json:"type,omitempty"`
	Args   []*Expr  `json:"args,omitempty"`
	Params []string `json:"params,omitempty"`
	Desc   bool     `json:"desc,omitempty"`
	Neg    bool     `json:"neg,omitempty"`  // add: 减
	Zone   string   `json:"zone,omitempty"` // add / trunc: 领域的时区(IANA 名;空 = UTC)
	// Loose: 只给差分测试的"错误实现"用(参考求值器认,SQL 后端和 IR 里没有)。ymd: 不合法的日期按日历进位
	// (20260230 → 2026-03-02、20260100 → 2025-12-31、20261301 → 2027-01-01;不检查就 make_date / time.Date 的实现)
	Loose bool `json:"-"`
}

// NowParam: now() 的值在参考求值器的 Params 里的键,也是 SQL 里的参数名(sqlc.arg(factory_now))。
// 适配器把它接到运行时的时钟(请求开始的时刻);请求(proto)里没有这个字段。
const NowParam = "factory_now"

// 时间的单位。
const (
	Minute = "minute"
	Hour   = "hour"
	Day    = "day"
	Month  = "month"
)

func Now() *Expr { return &Expr{Op: "now"} }

// YMD: 整数(YYYYMMDD)→ 日期(不合法 → NULL)。
func YMD(x *Expr) *Expr { return &Expr{Op: "ymd", Args: []*Expr{x}} }

// Add: x + n 个单位(neg: 减)。zone: 时间的 day / month 按哪个时区的日历。
func Add(x, n *Expr, unit string, neg bool, zone string) *Expr {
	return &Expr{Op: "add", Name: unit, Args: []*Expr{x, n}, Neg: neg, Zone: zone}
}

// Trunc: x 截到 unit 的开始(时间按 zone 的日历)。
func Trunc(x *Expr, unit, zone string) *Expr {
	return &Expr{Op: "trunc", Name: unit, Args: []*Expr{x}, Zone: zone}
}

// UsesNow: 表达式里有没有 now()。
func UsesNow(e *Expr) bool {
	if e == nil {
		return false
	}
	if e.Op == "now" {
		return true
	}
	for _, a := range e.Args {
		if UsesNow(a) {
			return true
		}
	}
	return false
}

// PipelineUsesNow: 流水线里有没有 now()(适配器要接时钟、测试要固定 now)。
func (p *Pipeline) UsesNow() bool {
	for _, st := range p.Steps {
		if UsesNow(st.Pred) {
			return true
		}
		for _, x := range st.Project {
			if UsesNow(x.Expr) {
				return true
			}
		}
	}
	return false
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
func Contains(x *Expr, param string) *Expr {
	return &Expr{Op: "contains", Name: param, Args: []*Expr{x}}
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
//	group         Keys + Aggs: 按 Keys 分组(NULL 视为同一组),每组输出一行 = 键的值 + 聚合值(agg.go)。
//	              没有输入行 → 没有输出行。输出的列 = Keys(原来的类型)+ Aggs(AggType)。一条流水线只能有一个
//	rank          Order + As: 按 Order 的名次(SQL 的 RANK(): 并列同名次,下一个名次跳过),加一列 As(int64)。
//	              只能在 group 之后。文本按字节;NULL 的位置和 sort 一样
//
// lookup / nest / child_agg 的对方可以是逻辑表(Source 不为空): 对方的行 = Source 这条子流水线的结果
// (来源单元的展开: scan 物理表 → project → filter → pick_per_key → …;只能用这几种元件,不能引用请求参数)。
// Columns = 子流水线的输出的列,Table = 逻辑表名(报错用)。逻辑表的唯一键 = 子流水线的 pick_per_key 的键(自然键)。
// 其他规则(NULL 不相等、找不到、上限、全序)和普通的表一样。nest 的全序: 唯一键就是取最新的键时,
// 可以为空的列也算(NULL 自己是一组,一个父行里最多一行)。
// SQL 后端: LATERAL 子查询,关联条件里对方的列是取最新的键时,下推到取最新之前(索引能用;等式见 pushdown.go)
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
	Aggs    []Agg       `json:"aggs,omitempty"`  // group / child_agg: 聚合(agg.go)
	As      string      `json:"as,omitempty"`    // rank: 名次的列名;nest: 子行的列表的列名
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
	// (Aggs / As 见上: child_agg 的聚合、nest 的列名)
	// Source: 对方是逻辑表时,对方的行 = 这条子流水线的结果(见上)
	Source []Step `json:"source,omitempty"`
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
func Group(keys []string, aggs ...Agg) Step {
	return Step{Kind: "group", Keys: keys, Aggs: aggs}
}
func Rank(as string, order ...SortKey) Step { return Step{Kind: "rank", As: as, Order: order} }

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
	var steps func(ss []Step) error
	steps = func(ss []Step) error {
		for i := range ss {
			if err := walk(ss[i].Pred); err != nil {
				return err
			}
			for _, x := range ss[i].Project {
				if err := walk(x.Expr); err != nil {
					return err
				}
			}
			if err := steps(ss[i].Source); err != nil {
				return err
			}
		}
		return nil
	}
	return steps(p.Steps)
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
