// Package sources 是来源(逻辑表 ← 物理存储)的注册表: 每种来源一个文件(table.go、json_versions.go)。
//
// 一种来源 = 领域里的写法 / 逻辑表的列 / 流水线的开头 / 验收数据怎么写进物理表。
// 新增一种来源 = 这里一个文件(spec.Storage 的字段现在是按 JSON版本 设计的,配置完全不同的来源要加字段)。
package sources

import (
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// Domain: 领域里这张表的写法(设计编译器实现)。
type Domain interface {
	Name() string         // 逻辑表名
	Node() *yaml.Node     // 这张表的映射
	PrimaryKey() []string // 主键
	CheckKeys(n *yaml.Node, where string, keys ...string)
	Err(line int, format string, args ...any)
	Column(name, desc, enum string)      // 登记逻辑列的说明 / 枚举
	Encoding(line int, name, enc string) // 登记逻辑列的编码(整数里的日期: YYYYMMDD;不认识的编码报错)
}

// Column: 逻辑表的列(注入 catalog,之后当普通的表用)。
type Column struct {
	Name, SQLType string
	NotNull       bool
}

// Physical: 验收数据的一行写进物理表时的样子。
type Physical struct {
	Values map[string]any // 列 → 值(登记顺序列由验收按类型合成)
	Order  bool           // 要不要合成登记顺序列(JSON版本: 行的顺序 = 登记的顺序)
}

type Source struct {
	Name       string   // 领域里 存储 的值: JSON版本(普通的表 = "")
	Kind       string   // spec / IR 的 storage.kind
	DesignKeys []string // 领域里这种来源可以写的键
	FieldKeys  []string // 字段可以写的键(有 字段 的来源)
	Parse      func(d Domain) *spec.Storage
	Columns    func(st *spec.Storage, nk []string) []Column
	Steps      func(st *spec.Storage, nk []string, scan core.Step, lib *cells.Library) ([]core.Step, error)
	// Row: 逻辑表的一行(验收数据)→ 物理表的一行;enumDB: 枚举名 → DB 值
	Row func(st *ir.Storage, row map[string]any, enumDB func(col string, v any) (any, error)) (Physical, error)
	// ShardKey: 物理表的一行里分库键的值(差分测试按它选库)
	ShardKey func(st *ir.Storage, column string, physical map[string]any) any
}

var (
	byName = map[string]*Source{}
	byKind = map[string]*Source{}
)

func Register(s *Source) {
	if byName[s.Name] != nil || byKind[s.Kind] != nil {
		panic("sources: duplicate " + s.Name + " / " + s.Kind)
	}
	byName[s.Name], byKind[s.Kind] = s, s
}

func ByName(n string) (*Source, bool) { s, ok := byName[n]; return s, ok }
func ByKind(k string) (*Source, bool) { s, ok := byKind[k]; return s, ok }

// Names: 领域里 存储 可以写的值。
// All: 全部来源(名字的顺序)。
func All() []*Source {
	var out []*Source
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func Names() string {
	var out []string
	for n := range byName {
		if n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " / ")
}

// AllKeys: 全部来源的领域键(普通的表不能写这些键)。
func AllKeys() []string {
	var out []string
	for _, s := range byName {
		out = append(out, s.DesignKeys...)
	}
	sort.Strings(out)
	return out
}
