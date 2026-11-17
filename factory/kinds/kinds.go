// Package kinds 是类型的注册表: 每种类型一个文件(int32.go、date.go …),
// 各层(设计语言 / 编译器 / 适配器 / validation / 差分测试)只查这里,不再各自写 if kind == "date"。
//
// 新增一种类型 = 新增一个文件 + 在文件里 Register。
package kinds

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"apifactory/factory/core"
)

// Boundary: validation 套件的一个边界值(proto 校验规则 → 违反 / 合法)。
type Boundary struct {
	Label string
	Value any
	Valid bool
}

type Type struct {
	Name  string    // 设计稿里的中文名: 日期
	Base  string    // 基于哪个类型(带格式的文本: 邮编 → 文本)。没写的字段从 Base 继承;SQL 类型名不继承(列的类型仍是 Base)
	Core  core.Kind // 核心类型(没有 = 只能作为计算的输出,例如 小数)
	SQL   []string  // SQL 的类型名(第一个是规范名,其余是别名: int4 / integer / serial)
	Proto string    // proto 的类型: string / int32 / google.protobuf.Timestamp
	Value string    // 例子的值的种类(设计编译器用): int / string / time / date / decimal / float / bool
	// Pattern: proto 的 string.pattern(格式由 protovalidate 检查),和它的边界值
	Pattern    string
	Boundaries []Boundary
	// Go: gen/db 里的 Go 类型
	Go string
	// Parse: 设计稿例子里的写法 → 值(nil = 原样)
	Parse func(s string) (any, error)
	// Domain: 差分测试的值域(故意包含陷阱值);Missing: 数据里不存在的值
	Domain  []any
	Missing any
}

var (
	byName  = map[string]*Type{}
	bySQL   = map[string]*Type{}
	all     []*Type
	pending []*Type
	once    sync.Once
)

// Register: 每个类型文件的 init 里调用。只登记;继承和索引在第一次查询时统一处理(不依赖文件的初始化顺序)。
func Register(t *Type) { pending = append(pending, t) }

// resolve: 先登记基本类型,再登记派生类型(继承 Base)。名字 / SQL 类型名重复 → panic(启动时就发现)。
func resolve() {
	once.Do(func() {
		for _, t := range pending {
			if t.Base == "" {
				index(t)
			}
		}
		for _, t := range pending {
			if t.Base != "" {
				b, ok := byName[t.Base]
				if !ok {
					panic("kinds: " + t.Name + ": base " + t.Base + " is not registered")
				}
				inherit(t, b)
				index(t)
			}
		}
	})
}

func index(t *Type) {
	if _, dup := byName[t.Name]; dup {
		panic("kinds: duplicate name " + t.Name)
	}
	byName[t.Name] = t
	for _, s := range t.SQL {
		if t.Base != "" {
			break // 派生类型不占用 SQL 类型名(text 列仍然是 文本)
		}
		if _, dup := bySQL[s]; dup {
			panic("kinds: duplicate SQL type " + s)
		}
		bySQL[s] = t
	}
	all = append(all, t)
}

func inherit(t, b *Type) {
	t.Core, t.SQL = b.Core, b.SQL
	if t.Proto == "" {
		t.Proto = b.Proto
	}
	if t.Value == "" {
		t.Value = b.Value
	}
	if t.Go == "" {
		t.Go = b.Go
	}
	if t.Parse == nil {
		t.Parse = b.Parse
	}
	if t.Domain == nil {
		t.Domain, t.Missing = b.Domain, b.Missing
	}
}

// ByName: 中文名 → 类型。

// BySQL: SQL 类型名(含别名)→ 类型。
func BySQL(sqlType string) (*Type, bool) {
	resolve()
	t, ok := bySQL[sqlType]
	return t, ok
}

// ByCore: 核心类型 → 类型(规范的那一个)。
func ByCore(k core.Kind) (*Type, bool) {
	resolve()
	for _, t := range all {
		if t.Core == k && t.Core != "" && t.Base == "" {
			return t, true
		}
	}
	return nil, false
}

// ByPattern: proto 的 string.pattern → 类型(validation 套件用)。
func ByPattern(p string) (*Type, bool) {
	resolve()
	for _, t := range all {
		if t.Pattern != "" && t.Pattern == p {
			return t, true
		}
	}
	return nil, false
}

// Names: 全部中文名(报错时列出)。
func Names() string {
	resolve()
	var out []string
	for _, t := range all {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return strings.Join(out, " / ")
}

// CanonicalSQL: 规范的 SQL 类型名。
func (t *Type) CanonicalSQL() string {
	if len(t.SQL) == 0 {
		panic(fmt.Sprintf("kinds: %s has no SQL type", t.Name))
	}
	return t.SQL[0]
}

// ByValue: 例子的值的种类 → 类型(第一个注册的;int32 / int64 共用 int 的解析)。
func ByValue(v string) (*Type, bool) {
	resolve()
	for _, t := range all {
		if t.Value == v && t.Base == "" {
			return t, true
		}
	}
	return nil, false
}

// ProtoPattern: proto 源码里 string.pattern 的写法(反斜杠转义)。
func (t *Type) ProtoPattern() string { return strings.ReplaceAll(t.Pattern, `\`, `\\`) }

// ByName: 中文名 → 类型。
func ByName(name string) (*Type, bool) {
	resolve()
	t, ok := byName[name]
	return t, ok
}
