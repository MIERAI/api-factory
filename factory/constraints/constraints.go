// Package constraints: 输入约束的注册表(接线层)。一种约束一个文件。
//
// 每种约束提供:
//   - 设计语言: 键(范围 / 长度 / 个数 / 格式 …)、能用在什么类型上、写法的检查
//   - proto: 生成的 protovalidate 规则
//   - 验收: 从 protovalidate 规则得到边界值(违反 / 合法)
//   - 差分: 被这个输入筛选的列的值域(请求要通过校验,数据要和请求的值域重合)
//
// 列表由框架统一处理: 元素级的约束(Level = Elem)在列表上用于每个元素(规则加 repeated.items. 前缀,
// 边界值包成一个元素的列表);列表级的约束(Level = List)只能用于列表。
//
// 规则在 IR 里保存为 protovalidate 的原始规则(JSON),所以加一种约束不需要改 IR。
package constraints

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/encoding/protojson"

	"apifactory/factory/ir"
)

type Level int

const (
	Elem Level = iota // 标量的值(列表时用于每个元素)
	List              // 列表本身
)

// Ctx: 约束用在哪个输入上。
type Ctx struct {
	ProtoType  string // 元素的 proto 类型: int32 / int64 / string / …
	List       bool   // 输入是不是列表(Elem 级的约束编译时总是 false: 框架负责前缀)
	Required   bool
	HasPattern bool // 类型本身带格式(日期 / 邮编 …)
}

// Boundary: 一个边界值。
type Boundary struct {
	Label string
	Value any
	Valid bool
	Unset bool // 不设置这个字段
}

// Domain: 差分测试里被筛选的列的值域。Missing = 数据里不存在、但请求合法的值(nil = 用默认)。
type Domain struct {
	Values  []any
	Missing any
	// Expand: 第 i 个合法的值(i 不同 → 值不同)。自然键需要很多不同的值时用(多版本来源);nil = 只能用 Values。
	Expand func(i int) any
}

// Constraint: 一种约束。Key 为空的约束不是设计语言写的(由类型带来: 枚举、带格式的类型)。
type Constraint struct {
	Key     string
	Level   Level
	Example string // 写法的例子(报错用)
	// Compile: 设计语言的值 → protovalidate 规则(不带 repeated.items. 前缀)。
	Compile func(c Ctx, value string) ([]string, error)
	// Boundaries: 从规则得到边界值。elem: 一个合法的元素(列表级的约束用)。返回覆盖的说明("" = 这个约束不在规则里)。
	Boundaries func(r *validate.FieldRules, f *ir.Field, elem any) ([]Boundary, string)
	// Domain: 差分的值域(可选)。
	Domain func(r *validate.FieldRules) *Domain
}

var registry []*Constraint

func register(c *Constraint) { registry = append(registry, c) }

// Keys: 设计语言里能写的约束的键。
func Keys() []string {
	var ks []string
	for _, c := range registry {
		if c.Key != "" {
			ks = append(ks, c.Key)
		}
	}
	sort.Strings(ks)
	return ks
}

// ValueOK: 这个键的值写法对不对(不知道输入的类型: 只要在某种类型上能编译就算对;类型不合由设计稿的编译报错)。
func ValueOK(key, value string) bool {
	c := byKey(key)
	if c == nil {
		return false
	}
	for _, ctx := range []Ctx{{ProtoType: "int32"}, {ProtoType: "int64"}, {ProtoType: "string"}, {ProtoType: "string", List: true}, {ProtoType: "int64", List: true}} {
		if c.Level == List && !ctx.List {
			continue
		}
		if c.Level == Elem {
			ctx.List = false
		}
		if _, err := c.Compile(ctx, value); err == nil {
			return true
		}
	}
	return false
}

// Forms: 设计语言里能写的约束: "键(例子)"。审查 prompt 用。
func Forms() []string {
	var out []string
	for _, k := range Keys() {
		out = append(out, fmt.Sprintf("%s(%s)", k, byKey(k).Example))
	}
	return out
}

// Compile: 一个输入的所有约束 → protovalidate 规则。values: 设计语言里写的 键 → 值。
func Compile(c Ctx, values map[string]string) ([]string, []error) {
	var rules []string
	var errs []error
	for _, k := range sortedKeys(values) {
		ct := byKey(k)
		if ct == nil {
			continue
		}
		switch {
		case ct.Level == List && !c.List:
			errs = append(errs, fmt.Errorf("%s 只能用于列表", k))
			continue
		case ct.Level == Elem && c.List:
			ec := c
			ec.List, ec.Required = false, false
			rs, err := ct.Compile(ec, values[k])
			if err != nil {
				errs = append(errs, fmt.Errorf("%s %q: %w(例: %s)", k, values[k], err, ct.Example))
				continue
			}
			for _, r := range rs {
				rules = append(rules, "repeated.items."+r)
			}
			continue
		}
		rs, err := ct.Compile(c, values[k])
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w(例: %s)", k, values[k], err, ct.Example))
			continue
		}
		rules = append(rules, rs...)
	}
	return rules, errs
}

// Has: 设计语言里写了这个键(框架以外的地方需要知道时用,例如 个数 会接管列表的 min_items)。
func Has(values map[string]string, key string) bool { _, ok := values[key]; return ok }

func byKey(k string) *Constraint {
	for _, c := range registry {
		if c.Key == k {
			return c
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Raw: protovalidate 规则 → IR 里保存的 JSON。
func Raw(r *validate.FieldRules) json.RawMessage {
	b, err := protojson.Marshal(r)
	if err != nil {
		return nil
	}
	return b
}

func parse(raw json.RawMessage) *validate.FieldRules {
	if len(raw) == 0 {
		return nil
	}
	r := &validate.FieldRules{}
	if err := protojson.Unmarshal(raw, r); err != nil {
		return nil
	}
	return r
}

// Boundaries: 一个请求字段的边界值(不含"必填",那是 proto 的结构)。base: 基础请求(列表的合法元素从这里取)。
func Boundaries(f *ir.Field, base map[string]any) ([]Boundary, []string) {
	if f.Rules == nil {
		return nil, nil
	}
	r := parse(f.Rules.Raw)
	if r == nil {
		return nil, nil
	}
	if !r.HasRepeated() {
		return scalarBoundaries(r, f, nil)
	}
	var b []Boundary
	var cov []string
	var elem any
	if xs, ok := base[f.Name].([]any); ok && len(xs) > 0 {
		elem = xs[0]
	}
	if items := r.GetRepeated().GetItems(); items != nil {
		eb, ec := scalarBoundaries(items, f, nil)
		for _, x := range eb {
			b = append(b, Boundary{"元素 " + x.Label, []any{x.Value}, x.Valid, false})
			if x.Valid && elem == nil {
				elem = x.Value
			}
		}
		for _, c := range ec {
			cov = append(cov, "元素 "+c)
		}
	}
	if elem == nil {
		elem = defaultElem(f)
	}
	if elem == nil {
		return b, append(cov, "列表的个数: 没有生成(找不到合法的元素)")
	}
	for _, c := range registry {
		if c.Level != List || c.Boundaries == nil {
			continue
		}
		if lb, label := c.Boundaries(r, f, elem); label != "" {
			b = append(b, lb...)
			cov = append(cov, label)
		}
	}
	return b, cov
}

func scalarBoundaries(r *validate.FieldRules, f *ir.Field, elem any) ([]Boundary, []string) {
	var b []Boundary
	var cov []string
	for _, c := range registry {
		if c.Level != Elem || c.Boundaries == nil {
			continue
		}
		if lb, label := c.Boundaries(r, f, elem); label != "" {
			b = append(b, lb...)
			cov = append(cov, label)
		}
	}
	return b, cov
}

func defaultElem(f *ir.Field) any {
	switch {
	case len(f.Values) > 0:
		for _, v := range f.Values {
			if v.Number != 0 {
				return v.Name
			}
		}
	case strings.HasPrefix(f.Kind, "int"):
		return int64(1)
	case f.Kind == "string":
		return "a"
	}
	return nil
}

// DomainOf: 差分测试里被这个字段筛选的列的值域(列表看元素的规则)。nil = 没有约束。
func DomainOf(f *ir.Field) *Domain {
	if f == nil || f.Rules == nil {
		return nil
	}
	r := parse(f.Rules.Raw)
	if r == nil {
		return nil
	}
	if r.HasRepeated() {
		if r = r.GetRepeated().GetItems(); r == nil {
			return nil
		}
	}
	for _, c := range registry {
		if c.Domain == nil {
			continue
		}
		if d := c.Domain(r); d != nil {
			return d
		}
	}
	return nil
}

// Required: 规则里是不是必填(required 或 列表至少 1 个)。
func Required(f *ir.Field) bool {
	if f == nil || f.Rules == nil {
		return false
	}
	if f.Rules.Required {
		return true
	}
	r := parse(f.Rules.Raw)
	return r != nil && r.GetRepeated().GetMinItems() > 0
}
