package spec

// 领域层(specs/<domain>/_domain.yaml): 一个领域里所有 API 共用的决定。
// 只补充 schema / proto 表达不了的东西: 自然键、policy、枚举的含义、不变量、数据策略、领域级审查决定。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const DomainVersion = "0.1"

type Domain struct {
	Path            string            `yaml:"-"`
	Raw             string            `yaml:"-"` // 原文(给审查员和实现者看)
	DomainVersion   string            `yaml:"domain_version"`
	Entities        map[string]Entity `yaml:"entities"`
	ReviewDecisions []ReviewDecision  `yaml:"review_decisions"`
}

type Entity struct {
	NaturalKey []string              `yaml:"natural_key"`
	Policy     string                `yaml:"policy"` // mutable / versioned
	Storage    *Storage              `yaml:"storage,omitempty"`
	ShardBy    string                `yaml:"shard_by,omitempty"` // 分库键的列(按店铺分库等)
	Columns    map[string]ColumnMeta `yaml:"columns"`
	Invariants []Invariant           `yaml:"invariants"`
}

// Storage: 逻辑表(实体名)和物理存储不一致时的映射。现在只有 json_versions:
//
//	物理表每次变更追加一行: 字段在 JSON 列里,操作列区分 登记/删除,按 order_by 决定先后。
//	逻辑表 = 每个自然键最新登记的那一行;最新的是删除 → 不存在;逻辑删除标记为真 → 不存在(不回退到旧版本)。
//	编译器把它展开成单元库的来源单元(JSON版本),之后的环节把它当普通的表。
type Storage struct {
	Kind          string         `yaml:"kind"`          // json_versions
	Table         string         `yaml:"table"`         // 物理表
	DataColumn    string         `yaml:"data_column"`   // data
	ActionColumn  string         `yaml:"action_column"` // action_type
	DeleteValue   int64          `yaml:"delete_value"`  // 1
	OrderBy       []string       `yaml:"order_by"`      // [registered_at, id](登记的先后,后面的赢)
	LogicalDelete *LogicalDelete `yaml:"logical_delete,omitempty"`
	Fields        []StorageField `yaml:"fields"` // 逻辑列(顺序 = 视图的列顺序)
}

type LogicalDelete struct {
	Key   string `yaml:"key"`   // JSON 里的键: deleted
	Value string `yaml:"value"` // 文本比较: "1"
}

type StorageField struct {
	Name string `yaml:"name"`          // 逻辑列名
	Key  string `yaml:"key,omitempty"` // JSON 里的键(默认 = name)
	Type string `yaml:"type"`          // SQL 类型: int4 / int8 / text / numeric / date / timestamptz / bool
	// Nullable: 自然键的列也可以为空(表定义里非必填的键列)。空值自己是一组;不声明时键列不为空(取数时去掉键为空的行)
	Nullable bool `yaml:"nullable,omitempty"`
	// Column: 外壳列 —— 不在 JSON 里,取物理表的这一列(registered_at 等)。逻辑行 = 最新的版本,值是这个版本的
	Column string `yaml:"column,omitempty"`
}

func (f StorageField) JSONKey() string {
	if f.Key != "" {
		return f.Key
	}
	return f.Name
}

type ColumnMeta struct {
	Enum      *EnumMeta `yaml:"enum,omitempty"`
	OnUnknown string    `yaml:"on_unknown,omitempty"` // unspecified
	// Encoding: 整数列里的日期的编码(YYYYMMDD)。和日期比较时列按 ymd 读成日期(不合法的整数 → 不命中),SQL 按整数比较
	Encoding string `yaml:"encoding,omitempty"`
}

type EnumMeta struct {
	Proto  string           `yaml:"proto"`  // proto enum 的全名
	Values map[int64]string `yaml:"values"` // DB 值 → proto enum 值名
}

// SortedDBValues: DB 值升序。
func (e *EnumMeta) SortedDBValues() []int64 {
	var out []int64
	for v := range e.Values {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

type Invariant struct {
	Rule     string `yaml:"rule"`
	Enforced bool   `yaml:"enforced"`
}

// DomainPath: specs/<domain>/<api>/spec.yaml → specs/<domain>/_domain.yaml
func DomainPath(specPath string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(specPath)), "_domain.yaml")
}

// LoadDomain: 文件不存在时返回 nil(领域层是可选的)。
func LoadDomain(path string) (*Domain, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d Domain
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d.Path, d.Raw = path, string(b)
	return &d, nil
}

// Column: 某张表某一列的领域元数据(没有时 nil)。
func (d *Domain) Column(table, column string) *ColumnMeta {
	if d == nil {
		return nil
	}
	e, ok := d.Entities[table]
	if !ok {
		return nil
	}
	c, ok := e.Columns[column]
	if !ok {
		return nil
	}
	return &c
}

// Check: 只做不需要 proto 的机械检查(和 proto enum 的一致性在 prepare 里检查)。
func (d *Domain) Check() []string {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if d.DomainVersion != DomainVersion {
		add("domain_version: unsupported %q (want %q)", d.DomainVersion, DomainVersion)
	}
	for name, e := range d.Entities {
		switch e.Policy {
		case "mutable", "versioned":
		default:
			add("entities.%s.policy must be mutable or versioned", name)
		}
		if len(e.NaturalKey) == 0 {
			add("entities.%s.natural_key is required", name)
		}
		if st := e.Storage; st != nil {
			if st.Kind != "json_versions" {
				add("entities.%s.storage.kind must be json_versions", name)
			}
			if st.Table == "" || st.DataColumn == "" || st.ActionColumn == "" || len(st.OrderBy) == 0 || len(st.Fields) == 0 {
				add("entities.%s.storage: table, data_column, action_column, order_by and fields are required", name)
			}
			for _, k := range e.NaturalKey {
				found := false
				for _, f := range st.Fields {
					found = found || f.Name == k
				}
				if !found {
					add("entities.%s.storage: natural key %s is not in fields", name, k)
				}
			}
		}
		for col, c := range e.Columns {
			if c.Enum != nil {
				if c.Enum.Proto == "" || len(c.Enum.Values) == 0 {
					add("entities.%s.columns.%s.enum: proto and values are required", name, col)
				}
				if c.OnUnknown != "unspecified" {
					add("entities.%s.columns.%s.on_unknown must be unspecified (fail is not supported yet)", name, col)
				}
			}
		}
		for i, inv := range e.Invariants {
			if inv.Rule == "" {
				add("entities.%s.invariants[%d].rule is required", name, i)
			}
		}
	}
	for i, r := range d.ReviewDecisions {
		if r.Question == "" || r.Decision == "" {
			add("review_decisions[%d]: question and decision are required", i)
		}
	}
	return errs
}

// 验收数据(fixtures)里 存储: JSON版本 的逻辑表的一行可以带这个键: 这一行是删除 / 逻辑删除。
// 行的顺序 = 登记的顺序(后面的行是后登记的)。
const (
	FixtureAction       = "$action"
	ActionDelete        = "delete"
	ActionLogicalDelete = "logical_delete"
)
