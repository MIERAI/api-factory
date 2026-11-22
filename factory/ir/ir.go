// Package ir 定义 API 计划(IR): 编译器的输出,下游所有环节(验收生成、审查、语言适配器)的唯一输入。
//
// 这是工厂内部最重要的契约:
//   - 和语言无关: 只有 proto 的字段/类型、SQL 的列/类型、模式的参数,没有任何 Go 的概念
//   - 是文件: 编译器写到 .factory/plan.json,下游各自读取,可以分开部署、分开优化
//   - 有版本号: 不兼容的变更必须升 Version
package ir

import (
	"encoding/json"

	"apifactory/factory/core"
	"fmt"
	"os"
)

const Version = "2" // 2: API 带核心代数的流水线(Pipeline)

// DefaultPath: 编译器输出的位置。
const DefaultPath = ".factory/plan.json"

type Plan struct {
	Version  string    `json:"version"`
	Services []Service `json:"services"`
	APIs     []API     `json:"apis"`
	Enums    []Enum    `json:"enums"` // 领域层的 enum 映射(DB 值 ↔ proto enum)
	Storages []Storage `json:"storages,omitempty"`
	Shards   []Shard   `json:"shards,omitempty"` // 分库的表(验收把数据写进分库键对应的库)
	// ShardWithParent: 分库: 随父 的表(没有分库键的列;验收把行写进父行的库 / 每个库都写一份)
	ShardWithParent []string `json:"shard_with_parent,omitempty"`
}

type Service struct {
	ProtoPackage string `json:"proto_package"` // factory.jobs.v1
	Name         string `json:"name"`          // JobService
	Domain       string `json:"domain"`        // jobs
	ProtoFile    string `json:"proto_file"`    // factory/jobs/v1/jobs.proto(相对 buf 模块根)
}

type API struct {
	ID           string            `json:"id"`        // jobs.list-jobs
	SpecPath     string            `json:"spec_path"` // specs/jobs/list-jobs/spec.yaml
	Domain       string            `json:"domain"`    // jobs
	ProtoPackage string            `json:"proto_package"`
	Service      string            `json:"service"`
	Method       string            `json:"method"`
	Summary      string            `json:"summary"` // proto 里 rpc 注释的第一行
	Pattern      string            `json:"pattern"` // get_by_id / list_keyset / compute / custom
	Table        string            `json:"table,omitempty"`
	Rename       map[string]string `json:"rename,omitempty"` // 响应字段 → 来源的列(输出改名;枚举的映射按来源的列查)

	Request  Message `json:"request"`
	Response Message `json:"response"`

	Query    Query          `json:"query"`
	Pipeline *core.Pipeline `json:"pipeline,omitempty"` // 声明式查询: 只含基本元件(逃生口的 SQL 没有)
	Filters  []Filter       `json:"filters,omitempty"`  // 声明式 datasource 才有

	GetByID *GetByID `json:"get_by_id,omitempty"`
	Keyset  *Keyset  `json:"keyset,omitempty"`
	ListAll *ListAll `json:"list_all,omitempty"`
	Shard   *Shard   `json:"shard,omitempty"` // 分库的表: 按请求字段 Field(= 列 Column 的等于条件)选库
	// ExitParams: 出口自己的参数(新出口不用改 IR;例如 前N条 的 n)
	ExitParams map[string]any `json:"exit_params,omitempty"`
	Compute    *Compute       `json:"compute,omitempty"`
	Command    *Command       `json:"command,omitempty"` // 命令工厂(写入): 没有 Query / Pipeline

	// BaseRequest: 第一个 code: OK 的人写用例的请求(自动生成校验边界用例时作为"其他字段都合法"的基础)
	BaseRequest map[string]any `json:"base_request,omitempty"`
}

type Message struct {
	Name      string   `json:"name"` // GetJobStatusRequest
	Fields    []Field  `json:"fields"`
	HasRules  bool     `json:"has_message_rules,omitempty"` // 有消息级 CEL 规则
	Relations []string `json:"relations,omitempty"`         // 消息级 CEL 规则的 id(工厂生成的: 关系名/参数…,factory/constraints)
}

func (m *Message) Field(name string) *Field {
	for i := range m.Fields {
		if m.Fields[i].Name == name {
			return &m.Fields[i]
		}
	}
	return nil
}

// Field.Kind: int32 / int64 / uint32 / uint64 / string / bool / double / float / enum / timestamp / message
type Field struct {
	Name     string      `json:"name"`
	Kind     string      `json:"kind"`
	Optional bool        `json:"optional,omitempty"` // proto3 optional(标量的显式 presence)
	Presence bool        `json:"presence,omitempty"` // optional 或 message 类型
	List     bool        `json:"list,omitempty"`
	Enum     string      `json:"enum,omitempty"` // enum 的全名
	Message  *Message    `json:"message,omitempty"`
	Rules    *Rules      `json:"rules,omitempty"`
	Values   []EnumValue `json:"enum_values,omitempty"` // enum 字段: proto 定义的全部值
}

// Rules: 请求字段的 protovalidate 规则。边界值 / 值域由约束的注册表(factory/constraints)从 Raw 读取,
// 加一种约束不用改这里。只有编译器自己要用的两项单独列出。
type Rules struct {
	Required bool            `json:"required,omitempty"`
	MinItems *uint64         `json:"min_items,omitempty"` // repeated: 至少几个(1 = 列表不能为空 → 参数不是可选的)
	Raw      json.RawMessage `json:"raw,omitempty"`
}

type Column struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // SQL 类型: int4 / int8 / text / timestamptz ...
	NotNull bool   `json:"not_null,omitempty"`
}

type Param struct {
	Name    string `json:"name"` // = 请求字段名(声明式);cursor_* / row_limit 由模式使用
	Type    string `json:"type"`
	NotNull bool   `json:"not_null,omitempty"` // sqlc.arg = true,sqlc.narg = false
}

type Query struct {
	Name        string   `json:"name"`             // = Method
	Cardinality string   `json:"cardinality"`      // one / many
	SQL         string   `json:"sql"`              // 最终的 SQL(db/queries/<id>.sql 的内容)
	Escape      bool     `json:"escape,omitempty"` // datasource.sql 逃生口
	Columns     []Column `json:"columns"`          // 结果列(sqlc 分析)
	Params      []Param  `json:"params"`           // 参数(sqlc 分析)
}

// MemberParam: 组的列表(in_tuples)的每个字段对应的查询参数(并行的列表)。
func MemberParam(field, column string) string { return field + "__" + column }

type Filter struct {
	Field      string   `json:"field"` // 请求字段名 = 查询参数名
	Column     string   `json:"column"`
	ColumnType string   `json:"column_type"`
	Op         string   `json:"op"`     // eq / gt / gte / lt / lte / prefix
	SQLOp      string   `json:"sql_op"` // = / > / ...
	Presence   bool     `json:"presence,omitempty"`
	Columns    []string `json:"columns,omitempty"` // 多列的筛选单元(有效期包含: 开始, 结束)
	Grouped    bool     `json:"grouped,omitempty"` // 分组之后的筛选(列 = 计算 / 名次的输出名)
}

type GetByID struct {
	NotFoundEmpty   bool   `json:"not_found_empty,omitempty"` // 找不到时 OK + found = false
	NotFoundMessage string `json:"not_found_message"`         // "job not found: {job_id}"
}

// ListAll: 多条(不分页)。超过 MaxRows 时报错(查询多取一行来判断)。
type ListAll struct {
	OrderBy    []string `json:"order_by"`
	Desc       bool     `json:"desc"`
	MaxRows    int32    `json:"max_rows"`
	ItemsField string   `json:"items_field"`
}

type Keyset struct {
	OrderBy     []string `json:"order_by"`
	Desc        bool     `json:"desc"`
	DefaultSize int32    `json:"default_size"`
	MaxSize     int32    `json:"max_size"`
	ItemsField  string   `json:"items_field"` // 响应里的 repeated message 字段
}

// Command: 命令工厂的一个写操作(R3-1)。语义(factory/command 的模型状态和运行时 txn 一致):
//   - insert: INSERT 写入的列 + 固定值 RETURNING 响应的列
//   - update: UPDATE SET 写入的列 + 固定值 [+ 版本 + 1] WHERE 主键 [AND 版本 = 请求的值] RETURNING …
//   - delete: DELETE WHERE 主键 [AND 版本 = 请求的值] RETURNING …
//
// 没有碰到行 → 同一个事务里再读: 不存在 → NOT_FOUND,存在 → 版本冲突(ABORTED,⚖️ D1)。
type Command struct {
	Op        string       `json:"op"` // insert / update / delete
	Table     string       `json:"table"`
	Key       []CommandCol `json:"key,omitempty"`   // 主键 ← 请求字段(update / delete)
	Write     []CommandCol `json:"write,omitempty"` // 列 ← 请求字段
	Lock      *CommandCol  `json:"lock,omitempty"`  // 乐观锁的版本列 ← 请求字段
	Set       []CommandSet `json:"set,omitempty"`   // 列 ← 固定值 / 请求开始的时刻
	Returning []Column     `json:"returning"`       // 响应的字段 = 这些列(写之后的行)
	NotFound  string       `json:"not_found,omitempty"`
	// 组合命令(R3-2,Op = command): 按顺序执行的写操作 + 响应字段的来源。上面的单个操作的字段不用
	Ops     []CommandOp  `json:"ops,omitempty"`
	Outputs []CommandOut `json:"outputs,omitempty"`
}

// Unmets: 组合命令里声明的业务原因码(条件不成立时)。
func (c *Command) Unmets() []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range c.Ops {
		if op.Unmet != "" && !seen[op.Unmet] {
			seen[op.Unmet] = true
			out = append(out, op.Unmet)
		}
	}
	return out
}

// CommandOp: 组合命令(R3-2)的一个写操作。语义(factory/command 的模型状态、运行时 txn、生成的代码一致):
//   - insert: INSERT 值 RETURNING …
//   - insert_idempotent: INSERT 值 ON CONFLICT (幂等键) DO NOTHING;冲突 → 读出已有的行(不写),新建 = false
//   - update: UPDATE SET 值 [, 版本 + 1] WHERE 主键 [AND 版本 = 输入] [AND 条件] RETURNING …;
//     没有碰到行 → 不存在 → NOT_FOUND / 版本不一致 → ABORTED / 条件不成立 → FAILED_PRECONDITION + Unmet
//   - delete: DELETE WHERE 主键 [AND 版本 = 输入] RETURNING …
//
// 任何一个操作失败 → 整个事务回滚,返回那个操作的错误。后面的操作可以引用前面操作的返回值(ref)。
type CommandOp struct {
	Name      string       `json:"name"`
	Kind      string       `json:"kind"`
	Table     string       `json:"table"`
	Key       []CommandVal `json:"key,omitempty"`
	Values    []CommandVal `json:"values,omitempty"`
	Lock      *CommandVal  `json:"lock,omitempty"`
	Conflict  []string     `json:"conflict,omitempty"`
	Cond      *CmdExpr     `json:"cond,omitempty"`
	Unmet     string       `json:"unmet,omitempty"`
	NotFound  string       `json:"not_found,omitempty"`
	Returning []Column     `json:"returning,omitempty"` // 后面的操作 / 输出要用的列(没有 = RETURNING 1)
	Created   bool         `json:"created,omitempty"`   // 输出要用 是否新建(insert_idempotent)
}

// CommandVal: 列 ← 表达式(Type / NotNull = 列的)。
type CommandVal struct {
	Column  string   `json:"column"`
	Type    string   `json:"type"`
	NotNull bool     `json:"not_null,omitempty"`
	Expr    *CmdExpr `json:"expr"`
}

// CmdExpr: 组合命令的表达式(值 / 条件)。Op:
//   - input: 请求字段 Name;ref: 操作 Ref 的返回值的列 Name;col: 这一行(update 的对象)的列 Name(更新之前的值)
//   - const: 字面值 Value(数字 / 文本 / true / false;枚举 = 数据库值);now: 请求开始的时刻
//   - "+" / "-": 加减;"=" / "<>" / "<" / "<=" / ">" / ">=": 比较(空 → 不成立);and: 全部成立
//
// Type: 这个值的 SQL 类型(input / const 跟着运算 / 比较的另一边或写入的列;枚举的输入 Column = 映射用的列)。
type CmdExpr struct {
	Op     string     `json:"op"`
	Name   string     `json:"name,omitempty"`
	Ref    string     `json:"ref,omitempty"`
	Value  string     `json:"value,omitempty"`
	Type   string     `json:"type,omitempty"`
	Table  string     `json:"table,omitempty"`  // input 是枚举时: 映射用的表.列
	Column string     `json:"column,omitempty"` //
	Args   []*CmdExpr `json:"args,omitempty"`
}

// CommandOut: 组合命令的响应字段 ← 操作的返回值(Column)或 是否新建(Created)。
type CommandOut struct {
	Field   string `json:"field"`
	Op      string `json:"op"`
	Table   string `json:"table"`
	Column  string `json:"column,omitempty"`
	Type    string `json:"type,omitempty"`
	NotNull bool   `json:"not_null,omitempty"`
	Created bool   `json:"created,omitempty"`
}

// CommandCol: 请求字段 → 列(Type = 列的 SQL 类型)。
type CommandCol struct {
	Field   string `json:"field"`
	Column  string `json:"column"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null,omitempty"`
}

// CommandSet: 列 = 固定值。Now = 请求开始的时刻;否则 Value 是字面值(枚举列 = 数据库值)。
type CommandSet struct {
	Column string `json:"column"`
	Type   string `json:"type"`
	Value  string `json:"value,omitempty"`
	Now    bool   `json:"now,omitempty"`
}

// UsesNow: 命令用到请求开始的时刻。
func (c *Command) UsesNow() bool {
	if c == nil {
		return false
	}
	for _, s := range c.Set {
		if s.Now {
			return true
		}
	}
	for _, op := range c.Ops {
		for _, v := range append(append([]CommandVal{}, op.Key...), op.Values...) {
			if v.Expr.UsesNow() {
				return true
			}
		}
		if op.Cond.UsesNow() {
			return true
		}
	}
	return false
}

// UsesNow: 表达式里有 now。
func (x *CmdExpr) UsesNow() bool {
	if x == nil {
		return false
	}
	if x.Op == "now" {
		return true
	}
	for _, a := range x.Args {
		if a.UsesNow() {
			return true
		}
	}
	return false
}

type Compute struct {
	LogicFunction string    `json:"logic_function"` // AI 实现的纯函数(语言无关的逻辑名,如 getJobSummaryLogic)
	Examples      []Example `json:"examples,omitempty"`
}

// Example: 式样书 logic_examples 的一项(直接调用逻辑函数的样例)。
type Example struct {
	Name    string           `json:"name"`
	Request map[string]any   `json:"request,omitempty"`
	Rows    []map[string]any `json:"rows"`
	Expect  map[string]any   `json:"expect"`
}

type Enum struct {
	Table       string      `json:"table"`
	Column      string      `json:"column"`
	ProtoEnum   string      `json:"proto_enum"`       // factory.jobs.v1.JobStatus
	Name        string      `json:"name"`             // JobStatus
	Parent      string      `json:"parent,omitempty"` // 嵌套 enum 的外层 message 名
	DBType      string      `json:"db_type"`          // int4
	Values      []EnumValue `json:"values"`           // DB 值 → proto 值
	Unspecified string      `json:"unspecified"`      // JOB_STATUS_UNSPECIFIED
	OnUnknown   string      `json:"on_unknown"`
}

type EnumValue struct {
	DB     int64  `json:"db,omitempty"`
	Name   string `json:"name"`
	Number int32  `json:"number"`
}

// Storage: 逻辑表 → 物理表(存储: JSON版本)。验收把逻辑表的数据行写成物理表的行(JSON + 操作列 + 登记顺序)。
type Storage struct {
	Kind               string         `json:"kind"` // 来源的种类(factory/sources): json_versions
	View               string         `json:"view"`
	Table              string         `json:"table"`
	NaturalKey         []string       `json:"natural_key"`
	DataColumn         string         `json:"data_column"`
	ActionColumn       string         `json:"action_column"`
	DeleteValue        int64          `json:"delete_value"`
	OrderBy            []string       `json:"order_by"`
	LogicalDeleteKey   string         `json:"logical_delete_key,omitempty"`
	LogicalDeleteValue string         `json:"logical_delete_value,omitempty"`
	Fields             []StorageField `json:"fields"`
}

type StorageField struct {
	Name string `json:"name"`
	Key  string `json:"key"`
	Type string `json:"type"`
	// Column: 外壳列(物理表的列,不在 JSON 里)
	Column string `json:"column,omitempty"`
}

func (p *Plan) Storage(view string) *Storage {
	for i := range p.Storages {
		if p.Storages[i].View == view {
			return &p.Storages[i]
		}
	}
	return nil
}

// Shard: 分库的表。Table = 式样书 / 验收数据里的表名(逻辑表),Column = 分库键的列。
type Shard struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Field  string `json:"field,omitempty"` // API 里: 固定分库键的请求字段
}

func (p *Plan) Shard(table string) *Shard {
	for i := range p.Shards {
		if p.Shards[i].Table == table {
			return &p.Shards[i]
		}
	}
	return nil
}

// WithParent: 表 table 是 分库: 随父。
func (p *Plan) WithParent(table string) bool {
	for _, t := range p.ShardWithParent {
		if t == table {
			return true
		}
	}
	return false
}

func (p *Plan) Enum(table, column string) *Enum {
	for i := range p.Enums {
		if p.Enums[i].Table == table && p.Enums[i].Column == column {
			return &p.Enums[i]
		}
	}
	return nil
}

func Save(p *Plan, path string) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func Load(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if p.Version != Version {
		return nil, fmt.Errorf("%s: plan version %q, this tool expects %q", path, p.Version, Version)
	}
	for i := range p.APIs {
		if pl := p.APIs[i].Pipeline; pl != nil {
			if err := pl.Normalize(); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", path, p.APIs[i].ID, err)
			}
		}
	}
	return &p, nil
}
