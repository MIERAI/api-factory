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
}

type Service struct {
	ProtoPackage string `json:"proto_package"` // factory.jobs.v1
	Name         string `json:"name"`          // JobService
	Domain       string `json:"domain"`        // jobs
	ProtoFile    string `json:"proto_file"`    // factory/jobs/v1/jobs.proto(相对 buf 模块根)
}

type API struct {
	ID           string `json:"id"`        // jobs.list-jobs
	SpecPath     string `json:"spec_path"` // specs/jobs/list-jobs/spec.yaml
	Domain       string `json:"domain"`    // jobs
	ProtoPackage string `json:"proto_package"`
	Service      string `json:"service"`
	Method       string `json:"method"`
	Summary      string `json:"summary"` // proto 里 rpc 注释的第一行
	Pattern      string `json:"pattern"` // get_by_id / list_keyset / compute / custom
	Table        string `json:"table,omitempty"`

	Request  Message `json:"request"`
	Response Message `json:"response"`

	Query        Query          `json:"query"`
	Pipeline     *core.Pipeline `json:"pipeline,omitempty"`      // 声明式查询: 只含基本元件(逃生口的 SQL 没有)
	Filters      []Filter       `json:"filters,omitempty"`       // 声明式 datasource 才有
	TableColumns []Column       `json:"table_columns,omitempty"` // datasource.table 的全部列(合成测试数据用)

	GetByID *GetByID `json:"get_by_id,omitempty"`
	Keyset  *Keyset  `json:"keyset,omitempty"`
	Compute *Compute `json:"compute,omitempty"`

	// BaseRequest: 第一个 code: OK 的人写用例的请求(自动生成校验边界用例时作为"其他字段都合法"的基础)
	BaseRequest map[string]any `json:"base_request,omitempty"`
}

type Message struct {
	Name     string  `json:"name"` // GetJobStatusRequest
	Fields   []Field `json:"fields"`
	HasRules bool    `json:"has_message_rules,omitempty"` // 消息级 CEL 规则(不自动生成用例)
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

// Rules: buf.validate 规则里自动生成边界用例需要的部分。
type Rules struct {
	Required bool    `json:"required,omitempty"`
	Type     string  `json:"type,omitempty"` // int32 / int64 / enum / string
	Gt       *int64  `json:"gt,omitempty"`
	Gte      *int64  `json:"gte,omitempty"`
	Lt       *int64  `json:"lt,omitempty"`
	Lte      *int64  `json:"lte,omitempty"`
	In       []int64 `json:"in,omitempty"`
	NotIn    []int64 `json:"not_in,omitempty"`
	MinLen   *uint64 `json:"min_len,omitempty"`
	MaxLen   *uint64 `json:"max_len,omitempty"`
	Defined  bool    `json:"defined_only,omitempty"`
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
	Select      []string `json:"select,omitempty"` // 声明式: SELECT 的列(合成数据用)
}

type Filter struct {
	Field      string `json:"field"` // 请求字段名 = 查询参数名
	Column     string `json:"column"`
	ColumnType string `json:"column_type"`
	Op         string `json:"op"`     // eq / gt / gte / lt / lte / prefix
	SQLOp      string `json:"sql_op"` // = / > / ...
	Presence   bool   `json:"presence,omitempty"`
}

type GetByID struct {
	NotFoundMessage string `json:"not_found_message"` // "job not found: {job_id}"
}

type Keyset struct {
	OrderBy     []string `json:"order_by"`
	Desc        bool     `json:"desc"`
	DefaultSize int32    `json:"default_size"`
	MaxSize     int32    `json:"max_size"`
	ItemsField  string   `json:"items_field"` // 响应里的 repeated message 字段
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
	return &p, nil
}
