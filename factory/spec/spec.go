// Package spec 定义式样书格式并做机械校验(式样书闸门)。
//
// v0.2 的原则: 式样书里的每一行都应该是"只有人才能做的决定"。
// 能从 proto / schema / 模式 / 目录结构推导出来的东西不写:
//   - API id        ← 目录(specs/<domain>/<name>/spec.yaml → <domain>.<name>)
//   - proto 文件    ← rpc 名(buf 描述符)
//   - summary       ← proto 里 rpc 的注释
//   - 查询名        ← rpc 的方法名
//   - cardinality   ← pattern(get_by_id → one,其他 → many)
//   - SQL           ← datasource.table + filters + 响应字段(声明不了时才用 datasource.sql)
//   - AI 可编辑文件 ← pattern + 方法名
//   - 平台/模式已经保证的行为(NOT_FOUND / INVALID_ARGUMENT / UNAVAILABLE) ← 不写进 behavior
package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const Version = "0.2"

type Spec struct {
	Path        string     `yaml:"-"`
	Domain      *Domain    `yaml:"-"` // specs/<domain>/_domain.yaml(可选)
	SpecVersion string     `yaml:"spec_version"`
	API         API        `yaml:"api"`
	Pattern     Pattern    `yaml:"pattern"`
	Datasource  Datasource `yaml:"datasource"`
	// Behavior: 只写业务规则。compute / custom 必填,模式 API 一般不需要
	Behavior   []Behavior `yaml:"behavior"`
	Acceptance Acceptance `yaml:"acceptance"`
	// LogicExamples: compute 模式专用。直接调用逻辑函数(不经过 DB),用于 DB 里造不出来的输入
	// (约束不允许的值、需要上万行才能出现的边界)。工厂生成单元测试。
	LogicExamples []LogicExample `yaml:"logic_examples"`
	// ReviewDecisions: 式样书审查提出的问题和人的决定(包括"不处理")。以后的审查不再提出
	ReviewDecisions []ReviewDecision `yaml:"review_decisions"`
}

type API struct {
	RPC         string `yaml:"rpc"`
	Owner       string `yaml:"owner"`
	RiskLevel   string `yaml:"risk_level"`
	Description string `yaml:"description"` // 可选: proto 注释之外的背景说明
}

type ReviewDecision struct {
	Question string `yaml:"question"`
	Decision string `yaml:"decision"`
}

// Pattern 决定 handler 由谁写:
//   - get_by_id   : 工厂生成。按请求字段查 1 行,查不到 → NOT_FOUND
//   - list_keyset : 工厂生成。筛选 + keyset 分页
//   - list_all    : 工厂生成。筛选 + 排序,不分页(有上限,超过时报错)
//   - compute     : 工厂生成 handler(映射 + 查询),AI 只写纯函数 <method>Logic(req, rows)
//   - custom      : AI 写整个 handler(逃生口)
type Pattern struct {
	Kind            string         `yaml:"kind"`
	NotFoundMessage string         `yaml:"not_found_message"`         // get_by_id: "job not found: {job_id}"
	NotFoundEmpty   bool           `yaml:"not_found_empty,omitempty"` // get_by_id: 找不到时返回 OK + found = false(不是 NOT_FOUND)
	OrderBy         []string       `yaml:"order_by"`                  // list_keyset: 游标列,最后一列必须唯一
	Direction       string         `yaml:"direction"`                 // desc / asc / mixed(每列的方向在 directions;分页不能 mixed)
	Directions      []string       `yaml:"directions,omitempty"`      // direction = mixed 时: 每列的方向(和 order_by 一一对应)
	PageSize        PageSize       `yaml:"page_size"`
	MaxRows         int32          `yaml:"max_rows,omitempty"`       // list_all: 最多返回的件数(超过时报错,不悄悄截断)
	Params          map[string]any `yaml:"params,omitempty"`         // 出口自己的参数(新出口不用改 spec)
	PickPerGroup    *PickPerGroup  `yaml:"pick_per_group,omitempty"` // list_all: 筛选之后每组取一条
	Group           *GroupBy       `yaml:"group,omitempty"`          // 多条 / 前N条 / 一览: 筛选之后分组聚合(之后的列 = 键 + 聚合)
	Rank            *RankBy        `yaml:"rank,omitempty"`           // 分组之后的名次(RANK(): 并列同名次,下一个名次跳过)
	Command         *Command       `yaml:"command,omitempty"`        // 命令(写入: insert / update / delete;命令工厂 R3)
}

// Command: 命令工厂的一个写操作(R3-1: 新建 / 按键更新 / 按键删除)。表 = datasource.table。
//   - Key: 主键的列 ← 请求字段(update / delete;必须正好是表的主键)
//   - Write: 写入的列 ← 请求字段(insert 的值 / update 的 SET)
//   - Lock: 乐观锁的版本列 ← 请求字段(update / delete;WHERE 版本 = 请求的值,update 时版本 + 1)
//   - Set: 不来自请求的值(固定值 / now() = 请求开始的时刻)
//
// 组合命令(R3-2,种类 command): Ops = 按顺序执行的写操作(一个事务),Output = 响应字段 ← 操作的返回值。
// 条件更新、幂等新建是 Ops 里的操作(update + Cond、insert_idempotent)。
type Command struct {
	Key   []Binding `yaml:"key,omitempty"`
	Write []Binding `yaml:"write,omitempty"`
	Lock  *Binding  `yaml:"lock,omitempty"`
	Set   []SetCol  `yaml:"set,omitempty"`

	Ops    []CommandOp  `yaml:"ops,omitempty"`
	Output []OutputFrom `yaml:"output,omitempty"`

	// R4(docs/design/r4-events-messages.md): 消息。
	//   Emit: 组合命令的 发出(操作全部成功时,同一个事务里写进 outbox)
	//   Receive / Each / Results: 消息处理器(种类 message): 收到的消息类型、每项(对消息的列表字段逐项执行 Ops)、
	//   结果(业务结果的分支和各自发出的消息)
	//   Contracts: 用到的消息契约(收到的 + 发出的;来自 消息.yaml / 共享的 contracts/)
	Emit      []Emit      `yaml:"emit,omitempty"`
	Receive   *MessageRef `yaml:"receive,omitempty"`
	Each      string      `yaml:"each,omitempty"`
	Results   []Result    `yaml:"results,omitempty"`
	Contracts []Contract  `yaml:"contracts,omitempty"`
}

// MessageRef: 消息类型 + 契约版本 + 生成的 proto 消息名(data)。
type MessageRef struct {
	Type    string `yaml:"type"`
	Version int    `yaml:"version"`
}

// Emit: 发出的一条消息: data 的字段 ← 表达式(和写入的值同一套写法;列表字段可以原样传 输入.列表)。
type Emit struct {
	Type   string    `yaml:"type"`
	Values []ColExpr `yaml:"values"` // Column = data 的字段
}

// Result: 消息处理器的一个结果(按顺序判定)。Name = 成功 / 业务原因码。
//   - 业务原因码 = 某个操作的 不满足: 那个操作的条件不成立 → 回滚已做的操作(保存点),发出这里的消息
//   - When(当): 操作全部做完之后判定(Any = 任一项);成立 → 回滚(不是 成功 的结果)/ 选这个分支(成功 的结果)
//   - DLQ(处理: 死信): 不提交,消息进 DLQ(需要人处理的异常)
type Result struct {
	Name string `yaml:"name"`
	When string `yaml:"when,omitempty"`
	Any  bool   `yaml:"any,omitempty"`
	Emit []Emit `yaml:"emit,omitempty"`
	DLQ  bool   `yaml:"dlq,omitempty"`
}

// SuccessResult: 结果 成功 的名字(例子的 结果 也这样写)。
const SuccessResult = "成功"

// Contract: 一个消息契约(消息.yaml 的一项)。字段的类型是设计语言的类型名(factory/kinds)。
// 共享目录的契约文件(JSON Schema)的 x-factory 也是这个结构(Proto 不写进去: 每个使用方自己生成)。
type Contract struct {
	Type    string          `yaml:"type" json:"type"`
	Kind    string          `yaml:"kind" json:"kind"` // event / command
	Version int             `yaml:"version" json:"version"`
	Subject string          `yaml:"subject" json:"subject"` // envelope.subject 取哪个字段
	Desc    string          `yaml:"desc,omitempty" json:"desc,omitempty"`
	Proto   string          `yaml:"proto,omitempty" json:"-"` // data 的 proto 消息名(同一个包里)
	Fields  []ContractField `yaml:"fields" json:"fields"`
}

// ContractField: 契约的一个字段。Type 列表 = 列表(Elem = 元素的字段,Unique = 元素的唯一键,Rules 的 个数 = 上限)。
type ContractField struct {
	Name     string            `yaml:"name" json:"name"`
	Type     string            `yaml:"type" json:"type"`
	Required bool              `yaml:"required,omitempty" json:"required,omitempty"`
	Desc     string            `yaml:"desc,omitempty" json:"desc,omitempty"`
	Rules    map[string]string `yaml:"rules,omitempty" json:"rules,omitempty"` // 约束(范围 / 长度 / 格式 / 个数)
	Elem     []ContractField   `yaml:"elem,omitempty" json:"elem,omitempty"`
	Unique   []string          `yaml:"unique,omitempty" json:"unique,omitempty"`
}

// CommandOp: 组合命令的一个写操作。表达式(Expr / Cond)是设计稿的写法规范化之后的文本
// (factory/command 的 ParseExpr: 输入.x / 操作名.列 / 列 / 数字 / '文本' / now() / + - / 比较 / 并且;枚举已换成数据库值)。
type CommandOp struct {
	Name     string    `yaml:"name"`                // 设计稿里的名字(后面的操作 / 输出用它引用这个操作的返回值)
	Kind     string    `yaml:"kind"`                // insert / insert_idempotent / update / delete
	Table    string    `yaml:"table"`               //
	Key      []ColExpr `yaml:"key,omitempty"`       // update / delete: 主键的列 ← 值(正好是表的主键)
	Values   []ColExpr `yaml:"values,omitempty"`    // insert: 插入的值;update: SET
	Lock     *ColExpr  `yaml:"lock,omitempty"`      // update / delete: 乐观锁的版本列 ← 输入(update 时版本 + 1)
	Conflict []string  `yaml:"conflict,omitempty"`  // insert_idempotent: 幂等键(表的主键或唯一约束的列)
	Cond     string    `yaml:"cond,omitempty"`      // update: 条件(不成立 → FAILED_PRECONDITION + Unmet)
	Unmet    string    `yaml:"unmet,omitempty"`     // 条件不成立时的业务原因码(例: INSUFFICIENT_STOCK)
	NotFound string    `yaml:"not_found,omitempty"` // update / delete: 行不存在时的消息
	// R4: 仅当(操作名.新建 = 前面的幂等新建真的插入了 / 操作名.已更新 = 前面的更新真的更新了);
	// Skip = 不满足: 跳过(条件不成立不是错误,这个操作不做;后面用 操作名.列 看到的是现在的行)
	Only string `yaml:"only,omitempty"`
	Skip bool   `yaml:"skip,omitempty"`
}

// ColExpr: 列 ← 表达式。
type ColExpr struct {
	Column string `yaml:"column"`
	Expr   string `yaml:"expr"`
}

// OutputFrom: 响应字段 ← 操作的返回值(Column = 那个操作的表的列;Created = 幂等新建的 是否新建)。
type OutputFrom struct {
	Field   string `yaml:"field"`
	Op      string `yaml:"op"`
	Column  string `yaml:"column,omitempty"`
	Created bool   `yaml:"created,omitempty"`
}

// Binding: 请求字段 → 列。
type Binding struct {
	Field  string `yaml:"field"`
	Column string `yaml:"column"`
}

// SetCol: 列 = 固定值。Value: "now()" = 请求开始的时刻;其他 = 字面值(枚举写 proto 的值名)。
type SetCol struct {
	Column string `yaml:"column"`
	Value  string `yaml:"value"`
}

// NowValue: SetCol.Value 的"请求开始的时刻"。
const NowValue = "now()"

// UsesNow: 命令的值用到请求开始的时刻(例子要固定 现在)。
func (c *Command) UsesNow() bool {
	if c == nil {
		return false
	}
	for _, s := range c.Set {
		if s.Value == NowValue {
			return true
		}
	}
	for _, op := range c.Ops {
		for _, x := range append(append([]ColExpr{}, op.Key...), op.Values...) {
			if strings.Contains(x.Expr, NowValue) {
				return true
			}
		}
		if strings.Contains(op.Cond, NowValue) {
			return true
		}
	}
	return false
}

// GroupBy: 按 keys 分组(NULL 是一组),每组一行 = 键 + 聚合。没有输入行 → 没有输出行。
type GroupBy struct {
	Keys []string `yaml:"keys"`
	Aggs []Agg    `yaml:"aggs,omitempty"`
}

// Agg: 输出名 = func(column)。func: count(不写 column)/ sum / min / max(规则: factory/core/agg.go)。
type Agg struct {
	Name   string `yaml:"name"`
	Func   string `yaml:"func"`
	Column string `yaml:"column,omitempty"`
}

// RankBy: 按 order("total desc")的名次,输出列 as。
type RankBy struct {
	Order []string `yaml:"order"`
	As    string   `yaml:"as"`
}

// PickPerGroup: 按 by 分组,组内按 order(列 + 方向)取第一行。
type PickPerGroup struct {
	By    []string `yaml:"by"`
	Order []string `yaml:"order"` // "valid_from desc"
}

type PageSize struct {
	Default int32 `yaml:"default"`
	Max     int32 `yaml:"max"`
}

// Datasource: 声明式(table + filters)或逃生口(sql),二选一。只支持读取。
type Datasource struct {
	Table string `yaml:"table"`
	// Filters: 请求字段 → 列 + 运算符。符合命名约定的可以省略:
	//   字段名 = 列名 → eq;  <x>_after → <x>_at gte;  <x>_before → <x>_at lt;  <单数表名>_id → id eq;
	//   <x>_prefix → x prefix(前缀匹配,% _ 按字面)
	// 有 presence 的字段(optional / Timestamp)未指定时不筛选;其他字段总是参与筛选
	Filters map[string]Filter `yaml:"filters"`
	// Columns: SELECT 的列。默认从响应字段推导;compute 的输入是逻辑函数用的列,需要显式写
	Columns []string `yaml:"columns"`
	// Rename: 响应字段 → 来源的列(字段名和列名不同时)。筛选 / 每组取一条 用列名,排序 用响应字段名(投影之后)
	Rename map[string]string `yaml:"rename,omitempty"`
	// Where: 固定条件(不来自请求;factory/where 的写法,列名是来源的列),和筛选一起作用
	Where []string `yaml:"where,omitempty"`
	// Lookups: 多对一查找(核心的 lookup),按顺序接在来源之后。加的列和来源的列一样,可以用在筛选 / 输出 / 排序里
	Lookups []Lookup `yaml:"lookups,omitempty"`
	// Nests: 一对多嵌套(核心的 nest),接在关联之后: 每一行加一列子行的列表
	Nests []Nest `yaml:"nests,omitempty"`
	// ChildAggs: 按父行聚合(核心的 child_agg),接在嵌套之后: 每一行加一列子行的聚合值
	ChildAggs []ChildAgg `yaml:"child_aggs,omitempty"`
	// TimeZone: 固定条件(来源 / 嵌套 / 子聚合)里的 今天 按这个时区(领域的时区)。用到 今天 / 现在 时才写
	TimeZone string `yaml:"time_zone,omitempty"`
	// Derived: 派生参数(不出现在请求里): 值由 now() 和请求参数计算(核心代数的时间运算),和筛选一样作用在来源的列上
	Derived map[string]Derived `yaml:"derived,omitempty"`
	// SQL: 逃生口(JOIN / 聚合 / 复杂条件)。写了 sql 就不写 filters/columns;
	// table 可以写,表示结果列来自哪张表(用于查领域层的 enum 映射)
	SQL string `yaml:"sql"`
}

// Lookup: 按键取另一张表(同一个库)的一行的列。On: "当前行的列 = 对方的键列"(多列 = 多项)。
// 对方的键列必须是主键或唯一约束(编译器的 DRC)。
type Lookup struct {
	Name    string    `yaml:"name"`              // 设计稿里的名字(报错 / 审查用)
	Table   string    `yaml:"table"`             // 对方的表
	On      []string  `yaml:"on"`                // "customer_id = id"
	Columns []TakeCol `yaml:"columns"`           // 加的列(新列名 ← 对方的列),按书写顺序
	Missing string    `yaml:"missing,omitempty"` // keep(默认): 加的列为空;drop: 丢弃这一行
}

// Nest: 子表(同一个库)里关联的行,作为子行的列表加一列 Name。On: "当前行的列 = 子表的列"(多列 = 多项)。
// OrderBy: "列 asc|desc",加上 On 的子表的列要覆盖子表的主键 / 唯一键(子行的顺序是全序;核心的 DRC)。
// Max: 子行的上限(超过 → FAILED_PRECONDITION,不截断)。
type Nest struct {
	Name    string    `yaml:"name"`            // 输出的列名(子行的列表)
	Table   string    `yaml:"table"`           // 子表
	On      []string  `yaml:"on"`              // "id = order_id"
	Where   []string  `yaml:"where,omitempty"` // 子表上的固定条件(factory/where 的写法,子表的列名)
	OrderBy []string  `yaml:"order_by"`        // "line_no asc"
	Max     int       `yaml:"max"`
	Columns []TakeCol `yaml:"columns"` // 子行的列(列名 ← 子表的列)
}

// ChildAgg: 关联的子行(经过 Where)的一个聚合,加一列 Name。Func: count / sum / min / max(count 没有 Column)。
type ChildAgg struct {
	Name   string   `yaml:"name"`
	Table  string   `yaml:"table"`
	On     []string `yaml:"on"`
	Where  []string `yaml:"where,omitempty"`
	Func   string   `yaml:"func"`
	Column string   `yaml:"column,omitempty"`
}

type TakeCol struct {
	Name string `yaml:"name"`
	From string `yaml:"from"`
}

// Derived: 一个派生参数。Expr 是核心代数的表达式(core.ParseExpr 的写法: now() / add(x, n, 单位) / trunc(x, 单位),
// 名字 = Inputs 里的请求参数);Column + Op 和筛选一样(Op = 筛选单元的内部名)。
type Derived struct {
	Expr     string            `yaml:"expr"`
	Column   string            `yaml:"column"`
	Op       string            `yaml:"op"`
	Inputs   map[string]string `yaml:"inputs,omitempty"`    // 用到的请求参数 → 核心类型(int32 / int64 / date / timestamp)
	TimeZone string            `yaml:"time_zone,omitempty"` // 时间的天 / 月、截到天 / 月按这个时区(领域的时区)
}

// DerivedInputs: 派生参数用到的请求参数(这些请求字段不是筛选条件)。
func (d Datasource) DerivedInputs() map[string]bool {
	out := map[string]bool{}
	for _, x := range d.Derived {
		for n := range x.Inputs {
			out[n] = true
		}
	}
	return out
}

type Filter struct {
	Column  string   `yaml:"column"`
	Columns []string `yaml:"columns,omitempty"` // 多列的筛选单元(有效期包含: [开始, 结束]);Column = 第一列
	Op      string   `yaml:"op"`                // eq / gt / gte / lt / lte / prefix
	// Grouped: 分组(+ 名次)之后的筛选(HAVING 一类): 列是计算 / 名次的输出名,接在 rank 之后、输出之前
	Grouped bool `yaml:"grouped,omitempty"`
}

// FilterOps: 运算符 → SQL。prefix 由 SQL 生成器特别处理(转义 % _ \,按字面匹配)。
var FilterOps = map[string]string{"eq": "=", "gt": ">", "gte": ">=", "lt": "<", "lte": "<=", "prefix": "LIKE", "contains": "LIKE"}

type LogicExample struct {
	Name    string         `yaml:"name"`
	Request map[string]any `yaml:"request"`
	// Rows: 逻辑函数的输入行。enum 列写 proto 值名;count: N 表示把这一行重复 N 次
	Rows   []map[string]any `yaml:"rows"`
	Expect map[string]any   `yaml:"expect"` // 响应字段的期待值;$absent = 未设置
}

type Behavior struct {
	When string `yaml:"when"`
	Then string `yaml:"then"`
}

type Acceptance struct {
	Fixtures map[string][]map[string]any `yaml:"fixtures"`
	Cases    []Case                      `yaml:"cases"`
}

type Case struct {
	Name    string         `yaml:"name"`
	Request map[string]any `yaml:"request"`
	Expect  Expect         `yaml:"expect"`
	// Steps 用于多步用例(如翻页)。request 里的 "$prev.<field>" 会被替换成上一步响应的字段
	Steps []Step `yaml:"steps"`
	// Now: 请求开始的时刻(RFC 3339)。用到 now() 的 API 的用例必须固定它: 验收执行器发请求头 X-Factory-Now,
	// 服务器只在 FACTORY_TEST_CLOCK=1(测试环境)时认这个头
	Now string `yaml:"now,omitempty"`
	// After: 命令的用例: 请求之后的数据(命令工厂;只看响应验证不了写操作)。每个用例从 fixtures 的状态开始
	After *After `yaml:"after,omitempty"`
	// Emitted: 写进 outbox 的消息(R4: 有 发出 的命令 / 消息处理器的用例必须写;[] = 什么都不发)
	Emitted *[]EmittedMsg `yaml:"emitted,omitempty"`
}

// EmittedMsg: 用例期待发出的一条消息(data 写出来的字段都比较;没写的字段不比较)。
type EmittedMsg struct {
	Type string         `yaml:"type"`
	Data map[string]any `yaml:"data"`
}

// After: 请求之后的状态。Unchanged = 所有表和请求之前一样;否则 Tables 里写出的表 = 之后的全部行
// (按主键对应,只比较写出来的列),没写出的表和请求之前一样。
type After struct {
	Unchanged bool                        `yaml:"unchanged,omitempty"`
	Tables    map[string][]map[string]any `yaml:"tables,omitempty"`
}

type Step struct {
	Request map[string]any `yaml:"request"`
	Expect  Expect         `yaml:"expect"`
}

// AllSteps 把单步用例也统一成 steps 形式。
func (c Case) AllSteps() []Step {
	if len(c.Steps) > 0 {
		return c.Steps
	}
	return []Step{{Request: c.Request, Expect: c.Expect}}
}

type Expect struct {
	Code     string         `yaml:"code"`
	Response map[string]any `yaml:"response"`
	// MessageContains: 错误时同时检查消息,防止"理由错误的通过"
	// (实测: 非法 page_token 的用例因为缺了必填参数而返回 INVALID_ARGUMENT,被误判为通过)
	MessageContains string `yaml:"message_contains,omitempty"`
}

// ---------------------------------------------------------------------------
// 推导出来的属性
// ---------------------------------------------------------------------------

// ID: specs/<domain>/<name>/spec.yaml → <domain>.<name>
func (s *Spec) ID() string {
	dir := filepath.Dir(s.Path)
	return filepath.Base(filepath.Dir(dir)) + "." + filepath.Base(dir)
}

func (s *Spec) PatternKind() string {
	if s.Pattern.Kind == "" {
		return "custom"
	}
	return s.Pattern.Kind
}

// AIWrites: 这个 API 是否有 AI 负责的部分。
func (s *Spec) AIWrites() bool {
	k := s.PatternKind()
	return k == "custom" || k == "compute"
}

// QueryName: 查询函数名 = rpc 方法名。
func (s *Spec) QueryName() string {
	p, _ := s.RPCParts()
	return p.Method
}

// Cardinality: 由出口决定(factory/exits 登记)。
func (s *Spec) Cardinality() string {
	if c, ok := patternCardinality[s.PatternKind()]; ok {
		return c
	}
	return "many"
}

// 出口的登记(factory/exits 的 init 调用;spec 不依赖 exits,避免循环)
var (
	patternChecks      = map[string]func(*Spec) []string{}
	patternCardinality = map[string]string{}
)

// RegisterPattern: 出口登记自己的规格检查。
func RegisterPattern(kind string, check func(*Spec) []string) { patternChecks[kind] = check }

// RegisterCardinality: 出口登记查询返回一行(one)还是多行(many)。
func RegisterCardinality(kind, c string) { patternCardinality[kind] = c }

// RPCParts 是 rpc 字段拆解后的结果。
type RPCParts struct {
	ProtoPackage string // factory.jobs.v1
	Service      string // JobService
	Method       string // GetJobStatus
}

var rpcRe = regexp.MustCompile(`^([a-z][a-z0-9_]*(?:\.[a-z][a-z0-9_]*)*)\.([A-Z][A-Za-z0-9]*)/([A-Z][A-Za-z0-9]*)$`)

func (s *Spec) RPCParts() (RPCParts, error) {
	m := rpcRe.FindStringSubmatch(s.API.RPC)
	if m == nil {
		return RPCParts{}, fmt.Errorf("api.rpc must look like pkg.v1.Service/Method, got %q", s.API.RPC)
	}
	return RPCParts{ProtoPackage: m[1], Service: m[2], Method: m[3]}, nil
}

// Domain 返回 proto 包里去掉前缀和版本号后的领域名(factory.jobs.v1 → jobs)。
func (p RPCParts) Domain() string {
	segs := strings.Split(p.ProtoPackage, ".")
	if len(segs) >= 2 && regexp.MustCompile(`^v\d+`).MatchString(segs[len(segs)-1]) {
		return segs[len(segs)-2]
	}
	return segs[len(segs)-1]
}

// Snake: GetJobStatus → get_job_status
func Snake(s string) string {
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

// ---------------------------------------------------------------------------
// 读取
// ---------------------------------------------------------------------------

func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Spec
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true) // 拼错或已废弃的字段直接报错
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.Path = path
	if s.Domain, err = LoadDomain(DomainPath(path)); err != nil {
		return nil, err
	}
	return &s, nil
}

// LoadAll 读取 dir 下所有 spec.yaml。
func LoadAll(dir string) ([]*Spec, error) {
	var specs []*Spec
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "spec.yaml" {
			s, err := Load(p)
			if err != nil {
				return err
			}
			specs = append(specs, s)
		}
		return nil
	})
	return specs, err
}

// ---------------------------------------------------------------------------
// 式样书闸门(只做不需要 proto / schema 的机械检查;那部分在 prepare 里检查)
// ---------------------------------------------------------------------------

var writeSQL = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|truncate|drop|alter|create|grant|revoke)\b`)

func (s *Spec) Check() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if s.Domain != nil {
		for _, e := range s.Domain.Check() {
			add("%s: %s", s.Domain.Path, e)
		}
	}
	if s.SpecVersion != Version {
		add("spec_version: unsupported %q (want %q)", s.SpecVersion, Version)
	}
	if s.API.Owner == "" {
		add("api.owner is required")
	}
	if _, err := s.RPCParts(); err != nil {
		add("%v", err)
	}
	switch s.API.RiskLevel {
	case "low", "medium", "high":
	default:
		add("api.risk_level must be low/medium/high")
	}

	// datasource: 声明式 / 逃生口 二选一
	ds := s.Datasource
	// 逃生口(sql)也可以写 table: 表示结果列来自哪张表(用于查领域层的 enum 等)
	declarative := len(ds.Filters) > 0 || len(ds.Columns) > 0 || len(ds.Derived) > 0
	switch {
	case ds.SQL != "" && declarative:
		add("datasource: filters/columns are for the declarative form; with sql, write the conditions in sql (table is allowed as metadata)")
	case ds.SQL == "" && ds.Table == "" && !(s.Pattern.Command != nil && len(s.Pattern.Command.Ops) > 0): // 组合命令: 操作写自己的表
		add("datasource: table (declarative) or sql (escape hatch) is required")
	}
	if ds.SQL != "" && writeSQL.MatchString(ds.SQL) {
		add("datasource.sql: only read queries are supported")
	}
	for field, f := range ds.Filters {
		if f.Column == "" {
			add("datasource.filters.%s: column is required", field)
		}
		if _, ok := FilterOps[f.Op]; !ok && f.Op == "" {
			add("datasource.filters.%s: op is required (eq/gt/gte/lt/lte/prefix,或单元库里的筛选单元名)", field)
		}
	}
	for name, d := range ds.Derived {
		if d.Expr == "" || d.Column == "" || d.Op == "" {
			add("datasource.derived.%s: expr, column and op are required", name)
		}
		if _, dup := ds.Filters[name]; dup {
			add("datasource.derived.%s: also a filter (a derived parameter is not a request field)", name)
		}
	}
	if s.PatternKind() == "compute" && ds.SQL == "" && len(ds.Columns) == 0 {
		add("datasource.columns: required for pattern compute (the logic function's input cannot be derived from the response)")
	}

	if len(s.LogicExamples) > 0 && s.PatternKind() != "compute" {
		add("logic_examples: only for pattern compute")
	}
	for i, e := range s.LogicExamples {
		if e.Name == "" || len(e.Expect) == 0 {
			add("logic_examples[%d]: name and expect are required", i)
		}
	}
	for i, d := range s.ReviewDecisions {
		if d.Question == "" || d.Decision == "" {
			add("review_decisions[%d]: question and decision are required", i)
		}
	}
	if s.AIWrites() && len(s.Behavior) == 0 {
		add("behavior: required for pattern %s (it is the spec of what AI implements)", s.PatternKind())
	}
	if len(s.Acceptance.Cases) == 0 {
		add("acceptance.cases: at least one case is required")
	}
	for i, c := range s.Acceptance.Cases {
		if c.Name == "" {
			add("acceptance.cases[%d]: name is required", i)
		}
		if len(c.Steps) > 0 && (c.Request != nil || c.Expect.Code != "") {
			add("acceptance.cases[%d]: use either request/expect or steps, not both", i)
		}
		for j, st := range c.AllSteps() {
			if st.Expect.Code == "" {
				add("acceptance.cases[%d].steps[%d]: expect.code is required", i, j)
			}
		}
	}
	if ds.Table != "" && ds.SQL == "" {
		allowed := map[string]bool{ds.Table: true}
		for _, l := range ds.Lookups { // 关联的表也可以(而且应该)准备数据
			allowed[l.Table] = true
		}
		for _, n := range ds.Nests { // 嵌套 / 子聚合的子表也是
			allowed[n.Table] = true
		}
		for _, a := range ds.ChildAggs {
			allowed[a.Table] = true
		}
		for table := range s.Acceptance.Fixtures {
			if !allowed[table] {
				add("acceptance.fixtures: table %s is not datasource.table (%s) or a lookup table", table, ds.Table)
			}
		}
	}
	for i, n := range ds.Nests {
		if n.Name == "" || n.Table == "" || len(n.On) == 0 || len(n.OrderBy) == 0 || n.Max <= 0 || len(n.Columns) == 0 {
			add("datasource.nests[%d]: name / table / on / order_by / max (> 0) / columns are required", i)
		}
		if ds.SQL != "" {
			add("datasource.nests[%d]: nests need a declarative datasource (not sql)", i)
		}
	}
	for i, a := range ds.ChildAggs {
		if a.Name == "" || a.Table == "" || len(a.On) == 0 || a.Func == "" {
			add("datasource.child_aggs[%d]: name / table / on / func are required", i)
		}
		if ds.SQL != "" {
			add("datasource.child_aggs[%d]: child_aggs need a declarative datasource (not sql)", i)
		}
	}
	for i, l := range ds.Lookups {
		if l.Name == "" || l.Table == "" || len(l.On) == 0 || len(l.Columns) == 0 {
			add("datasource.lookups[%d]: name / table / on / columns are required", i)
		}
		if l.Missing != "" && l.Missing != "keep" && l.Missing != "drop" {
			add("datasource.lookups[%d]: missing must be keep or drop", i)
		}
		if ds.SQL != "" {
			add("datasource.lookups[%d]: lookups need a declarative datasource (not sql)", i)
		}
	}

	// 每个出口的检查由出口自己登记(factory/exits)
	if check, ok := patternChecks[s.PatternKind()]; ok {
		for _, m := range check(s) {
			add("%s", m)
		}
	} else {
		add("pattern.kind %q 不存在(出口的注册表里没有;factory/exits)", s.PatternKind())
	}
	return errs
}

// ---------------------------------------------------------------------------
// 自动生成的验收用例(gen/acceptance/<id>.yaml)
//   人写的用例验证"意图";自动生成的用例验证"机制"(工厂和平台有没有把声明正确地变成代码)
// ---------------------------------------------------------------------------

type AutoAcceptance struct {
	Spec     string   `yaml:"spec"`
	Coverage []string `yaml:"coverage"` // 自动覆盖了哪些方面(给人和审查员看)
	Suites   []Suite  `yaml:"suites"`
}

// Suite: Fixtures 为 nil 时沿用当前 DB 内容(不重新灌数据)。
type Suite struct {
	Name     string                      `yaml:"name"`
	Fixtures map[string][]map[string]any `yaml:"fixtures,omitempty"`
	Cases    []Case                      `yaml:"cases"`
}

func AutoAcceptancePath(id string) string { return "gen/acceptance/" + id + ".yaml" }

func LoadAutoAcceptance(id string) (*AutoAcceptance, error) {
	b, err := os.ReadFile(AutoAcceptancePath(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a AutoAcceptance
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", AutoAcceptancePath(id), err)
	}
	return &a, nil
}
