package constraints

// 输入之间的规则(关系)的注册表。一种关系一个文件(rel_*.go)。
//
// 每种关系提供:
//   - 设计语言: 写法(正则)和例子,例如 "A 早于 B"
//   - proto: 消息级的 CEL 规则。id 写成 "关系名.参数.…(buf 只允许 a-z 0-9 . _ -)",编译器从 proto 读回 IR(ir.Message.Relations)
//   - 验收: 边界用例(一个请求改几个字段: 违反 / 合法)
//
// 手写的 proto 里 id 不是这种写法的 CEL 规则,工厂不认识 → 仍然需要人写用例。

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"apifactory/factory/ir"
)

// RelField: 关系用到的输入(设计语言编译时)。
type RelField struct {
	Name     string
	Kind     string // time / int / string / enum / …(设计语言的种类)
	List     bool
	Optional bool // 标量的 presence(proto3 optional)
	Required bool
}

// Absent: "这个输入没有指定"的 CEL(必填的输入 → "";规则要在没指定时放行)。
func (f RelField) Absent() string {
	switch {
	case f.Required:
		return ""
	case f.List:
		return fmt.Sprintf("size(this.%s) == 0", f.Name)
	case f.Kind == "time" || f.Optional:
		return fmt.Sprintf("!has(this.%s)", f.Name)
	}
	return ""
}

// Present: "这个输入指定了"的 CEL。
func (f RelField) Present() string {
	if a := f.Absent(); a != "" {
		if f.List {
			return fmt.Sprintf("size(this.%s) > 0", f.Name)
		}
		return fmt.Sprintf("has(this.%s)", f.Name)
	}
	return "true"
}

// Guard: 任何一个输入没指定时规则不检查。返回 "!has(a) || !has(b) || "(都必填时为空)。
func Guard(fs ...RelField) string {
	var g []string
	for _, f := range fs {
		if a := f.Absent(); a != "" {
			g = append(g, a)
		}
	}
	if len(g) == 0 {
		return ""
	}
	return strings.Join(g, " || ") + " || "
}

// CEL: 编译结果。
type CEL struct {
	Args    []string // 写进 id: 关系名.参数…
	Message string
	Expr    string
}

// RelCase: 一个边界用例。Set: 要设置的字段;Unset: 要去掉的字段。
type RelCase struct {
	Label string
	Set   map[string]any
	Unset []string
	Valid bool
}

// Relation: 一种输入之间的规则。
type Relation struct {
	Name    string
	Pattern *regexp.Regexp // 设计语言的写法(子匹配 = 参数)
	Example string
	Compile func(field func(string) (RelField, error), m []string) (CEL, error)
	// Cases: 边界用例。value(字段名) = 这个字段的一个合法的值(基础请求里的,或者约束给的)。
	Cases func(args []string, value func(string) any) []RelCase
}

var relations []*Relation

func registerRelation(r *Relation) { relations = append(relations, r) }

// CompileRelation: 设计语言的一条规则 → CEL(id = 关系名.参数…)。
func CompileRelation(text string, field func(string) (RelField, error)) (id string, c CEL, err error) {
	for _, r := range relations {
		m := r.Pattern.FindStringSubmatch(strings.TrimSpace(text))
		if m == nil {
			continue
		}
		c, err := r.Compile(field, m)
		if err != nil {
			return "", CEL{}, err
		}
		return strings.Join(append([]string{r.Name}, c.Args...), "."), c, nil
	}
	var ex []string
	for _, r := range relations {
		ex = append(ex, fmt.Sprintf("%q", r.Example))
	}
	return "", CEL{}, fmt.Errorf("无法识别。目前支持: %s", strings.Join(ex, " / "))
}

// RelationForms: 设计语言里能写的关系(例子)。审查 prompt 用。
func RelationForms() []string {
	var out []string
	for _, r := range relations {
		out = append(out, r.Example)
	}
	return out
}

// MatchRelation: 这条规则的写法是不是注册过的关系(不检查输入名)。
func MatchRelation(text string) bool {
	for _, r := range relations {
		if r.Pattern.MatchString(strings.TrimSpace(text)) {
			return true
		}
	}
	return false
}

// RelationOf: proto 里 CEL 规则的 id → 关系和参数(不认识 → nil)。
func RelationOf(id string) (*Relation, []string) {
	parts := strings.Split(id, ".")
	for _, r := range relations {
		if r.Name == parts[0] && len(parts) > 1 {
			return r, parts[1:]
		}
	}
	return nil, nil
}

// RelationCases: 请求消息的关系 → 边界用例。base: 基础请求。unknown: 工厂不认识的规则数(需要人写用例)。
func RelationCases(m *ir.Message, base map[string]any) (cases []RelCase, labels []string, unknown int) {
	for _, id := range m.Relations {
		r, args := RelationOf(id)
		if r == nil {
			unknown++
			continue
		}
		value := func(name string) any { return validValue(m, name, base) }
	next:
		for _, c := range r.Cases(args, value) {
			for _, u := range c.Unset { // 去掉必填字段的请求本来就不合法: "合法"的用例不成立
				if c.Valid && Required(m.Field(u)) {
					continue next
				}
			}
			cases = append(cases, c)
		}
		labels = append(labels, id)
	}
	return cases, labels, unknown
}

// validValue: 一个字段的合法值: 基础请求里的 → 约束给的合法边界值 → 按种类的默认值。
func validValue(m *ir.Message, name string, base map[string]any) any {
	if v, ok := base[name]; ok && v != nil {
		return v
	}
	for i := range m.Fields {
		f := &m.Fields[i]
		if f.Name != name {
			continue
		}
		bs, _ := Boundaries(f, base)
		for _, b := range bs {
			if b.Valid && !b.Unset {
				return b.Value // 列表的边界值本来就是列表
			}
		}
		if f.List {
			if e := defaultElem(f); e != nil {
				return []any{e}
			}
			return nil
		}
		switch {
		case f.Kind == "timestamp":
			return "2026-01-10T00:00:00Z"
		case strings.HasPrefix(f.Kind, "int"):
			return int64(1)
		default:
			return defaultElem(f)
		}
	}
	return nil
}

// shift: 时间(RFC 3339 的字符串)或整数加上 d(整数: 秒数当作 1)。
func shift(v any, d time.Duration) any {
	switch x := v.(type) {
	case string:
		t, err := time.Parse(time.RFC3339Nano, x)
		if err != nil {
			if day, err2 := time.Parse(time.DateOnly, x); err2 == nil { // 日期: 按天
				return day.Add(d).Format(time.DateOnly)
			}
			return x
		}
		return t.Add(d).UTC().Format(time.RFC3339Nano)
	case int64:
		return x + int64(d/time.Second)
	case int:
		return int64(x) + int64(d/time.Second)
	case float64:
		return int64(x) + int64(d/time.Second)
	}
	return v
}
