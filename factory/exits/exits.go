// Package exits 是出口(API 的种类)的注册表: 每个出口一个文件(get_by_id.go、list_all.go …)。
//
// 一个出口的定义 = 设计稿的写法 / 规格检查 / 请求和响应的形状 / IR / 编译时的流水线部件 / 差分测试的期待值。
// 各层只查这里。语言相关的部分(Go 的 handler 模板)在各适配器里,同样每个出口一个文件。
//
// 新增一个出口 = 这里一个文件 + 每个语言适配器一个文件。
package exits

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// Field: 工厂加到请求 / 响应里的字段(设计编译器生成 proto 用)。Kind = 例子的值的种类。
type Field struct {
	Name, Type, Kind, Desc string
	Rules                  []string
}

// Design: 设计稿里这个 API 的键(设计编译器实现)。
type Design interface {
	Str(key string) string
	Node(key string) *yaml.Node
	Order(key string) (cols []string, desc bool, err error) // 排序: [a 小到大, b 小到大]
	Err(format string, args ...any)
}

// PartsCtx: 编译时的上下文(编译器实现)。
type PartsCtx interface {
	ColumnKind(col string) (core.Kind, error) // 数据来源的列的核心类型
}

// DiffCtx: 差分测试的上下文(验收实现)。
type DiffCtx interface {
	API() *ir.API
	Rand() *rand.Rand
	Item(r core.Row, msg *ir.Message) map[string]any // 一行 → 期待的响应项
	Request(ps core.Params) map[string]any           // 参数 → 请求
	Requests() []core.Params                         // 随机请求(和主循环同一种生成方式)
	Invalid(req map[string]any) (bool, error)        // 请求按 proto 的规则是否不合法
	Eval(ps core.Params) ([]core.Row, error)         // 参考求值器
	UserParams() []core.ParamDecl                    // 请求里的筛选参数
	Column(param string) string                      // 参数筛选的列
	Anchors() []core.Row                             // 来源部分的行(参数取值用)
	Present(column string, v any) any                // 值 → 请求 / 期待值里的表示
	Missing(param string, i int) any                 // 参数的第 i 个"数据里不存在的值"(没有更多 → nil)
}

// Case: 出口自己的差分用例(名字 + 用例)。
type Case struct {
	Name string
	Case spec.Case
	Fail string // 不为空 = 这个用例本身做不出来(覆盖不足等),直接算失败
}

type Exit struct {
	Kind        string // spec 的 pattern.kind: list_all
	Name        string // 设计稿的 种类: 多条
	Cardinality string // one / many(查询函数返回一行还是多行)
	Cell        string // 单元库里的模式(套装)。空 = 没有声明式的查询(自定义)
	AI          bool   // AI 写的部分(计算 / 自定义)

	DesignKeys []string                        // 这个种类在设计稿里可以写的键
	Parse      func(d Design, p *spec.Pattern) // 设计稿 → spec 的 pattern
	Check      func(s *spec.Spec) []string     // 规格检查
	Request    func(p spec.Pattern) []Field    // 工厂加到请求里的字段(不是筛选条件)
	Rows       bool                            // 响应 = 行的列表(+ ResponseExtra);否则响应的字段 = 列
	RowsDesc   func(p spec.Pattern) string     // 行的列表字段的说明
	Extra      []Field                         // 响应里另外的字段(next_page_token)
	ItemKey    func(p spec.Pattern) string     // 例子的简写 [4, 3, 2] 用的列
	FromTable  bool                            // 响应的字段来自数据来源的列(单条);否则来自设计稿的 输出(计算)
	IR         func(s *spec.Spec, api *ir.API) error
	Parts      func(s *spec.Spec, x PartsCtx, parts *cells.Parts, p *core.Pipeline) error

	// 差分测试(nil = 这个出口没有差分测试)
	DiffParams   func(api *ir.API) core.Params                                                          // 求值时另外的参数(row_limit)
	DiffCase     func(x DiffCtx, rows []core.Row, req map[string]any) (c spec.Case, detail, tag string) // 一个请求 → 用例 + 说明 + 覆盖标签
	DiffMulti    bool                                                                                   // 覆盖条件: 要出现多行的结果
	DiffExtra    func(x DiffCtx) []Case                                                                 // 出口自己的用例(分页: 途中改条件 …)
	DiffCover    func(api *ir.API, tags map[string]int) string                                          // 覆盖信息里另外的说明(多条: 超过上限几次)
	EmptyRequest bool                                                                                   // 没有人写用例时,空请求可以作为 validation 的基础请求(分页)
	NotNullOrder bool                                                                                   // 排序列必须不为空(分页的游标)
	DiffRowsNeed func(api *ir.API) (minRows, requests int)                                              // 出口要的数据: 结果 ≥ minRows 行的合法请求至少 requests 个
}

var (
	byKind = map[string]*Exit{}
	byName = map[string]*Exit{}
)

// Register: 每个出口文件的 init 里调用。
func Register(e *Exit) {
	if byKind[e.Kind] != nil || byName[e.Name] != nil {
		panic("exits: duplicate " + e.Kind + " / " + e.Name)
	}
	byKind[e.Kind], byName[e.Name] = e, e
	spec.RegisterCardinality(e.Kind, e.Cardinality)
	if e.Check != nil {
		spec.RegisterPattern(e.Kind, e.Check)
	} else {
		spec.RegisterPattern(e.Kind, func(*spec.Spec) []string { return nil })
	}
}

func ByKind(k string) (*Exit, bool) { e, ok := byKind[k]; return e, ok }
func ByName(n string) (*Exit, bool) { e, ok := byName[n]; return e, ok }

// Names: 设计稿里可以写的全部 种类。
func Names() string {
	var out []string
	for n := range byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, " / ")
}

// All: 全部出口(种类名排序)。
func All() []*Exit {
	var out []*Exit
	for _, e := range byKind {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// RequestNames: 工厂加到请求里的字段名(不是筛选条件)。
func (e *Exit) RequestNames(p spec.Pattern) map[string]bool {
	out := map[string]bool{}
	if e.Request != nil {
		for _, f := range e.Request(p) {
			out[f.Name] = true
		}
	}
	return out
}

// ItemsField: 响应里行的列表字段(Rows 的出口)。
func ItemsField(api *ir.API, kind string) (string, error) {
	name := ""
	for _, f := range api.Response.Fields {
		if f.List && f.Kind == "message" {
			if name != "" {
				return "", fmt.Errorf("pattern %s: %s must have exactly one repeated message field", kind, api.Response.Name)
			}
			name = f.Name
		}
	}
	if name == "" {
		return "", fmt.Errorf("pattern %s: %s has no repeated message field", kind, api.Response.Name)
	}
	return name, nil
}

// orderFrom: spec 的排序 → 核心的 SortKey。
func orderFrom(p spec.Pattern) []core.SortKey {
	var out []core.SortKey
	for _, c := range p.OrderBy {
		out = append(out, core.SortKey{Col: c, Desc: p.Direction == "desc"})
	}
	return out
}

func direction(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

// lastOrder: 例子的简写用排序的最后一列(唯一)。
func lastOrder(p spec.Pattern) string {
	if len(p.OrderBy) == 0 {
		return ""
	}
	return p.OrderBy[len(p.OrderBy)-1]
}
