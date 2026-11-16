package core

// 类型检查(DRC 的一部分): 流水线每一步之后的列和类型;表达式的类型。
// 两个后端都依赖它(SQL 后端要知道哪里是文本 → COLLATE "C";参考求值器要知道参数的类型)。

import (
	"fmt"
	"strings"
)

// Schema: 某一步之后的行的列(有序)。
type Schema []NamedType

func (s Schema) Col(name string) (Type, bool) {
	for _, c := range s {
		if c.Name == name {
			return c.Type, true
		}
	}
	return Type{}, false
}

// Check: 检查流水线,返回最后的列。错误全部收集。
func Check(p *Pipeline) (Schema, error) {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	var cur Schema
	for i, st := range p.Steps {
		at := fmt.Sprintf("step %d (%s)", i+1, st.Kind)
		if st.From != "" {
			at += " [" + st.From + "]"
		}
		if i == 0 && st.Kind != "scan" {
			add("%s: 流水线必须以 scan 开始", at)
			break
		}
		switch st.Kind {
		case "scan":
			if i != 0 {
				add("%s: scan 只能在第一步", at)
			}
			cur = append(Schema(nil), st.Columns...)
		case "filter":
			t, err := p.typeOf(st.Pred, cur)
			if err != nil {
				add("%s: %v", at, err)
			} else if t.Kind != Bool {
				add("%s: 条件必须是 bool,实际是 %s", at, t.Kind)
			}
			cur = narrow(cur, st.Pred)
		case "project":
			var next Schema
			seen := map[string]bool{}
			for _, x := range st.Project {
				if seen[x.Name] {
					add("%s: 列 %s 重复", at, x.Name)
				}
				seen[x.Name] = true
				t, err := p.typeOf(x.Expr, cur)
				if err != nil {
					add("%s: 列 %s: %v", at, x.Name, err)
					continue
				}
				next = append(next, NamedType{x.Name, t})
			}
			cur = next
		case "pick_per_key":
			if len(st.Keys) == 0 || len(st.Order) == 0 {
				add("%s: 需要 keys 和 order", at)
			}
			for _, k := range st.Keys {
				t, ok := cur.Col(k)
				if !ok {
					add("%s: 键 %s 不存在", at, k)
				} else if t.Nullable {
					add("%s: 键 %s 可能为 NULL(键必须 NOT NULL)", at, k)
				}
			}
			checkOrder(cur, st.Order, at, add)
		case "sort":
			checkOrder(cur, st.Order, at, add)
		case "limit":
			if st.Param != "" {
				d := p.Param(st.Param)
				if d == nil || d.List || !d.Required || (d.Type.Kind != Int32 && d.Type.Kind != Int64) {
					add("%s: 参数 %s 必须是必填的整数", at, st.Param)
				}
			} else if st.N <= 0 {
				add("%s: N 必须 > 0", at)
			}
		default:
			add("%s: 不认识的元件", at)
		}
	}
	// 排序之后只能接 limit(否则顺序没有意义,SQL 也不保证子查询的顺序)
	for i, st := range p.Steps {
		if st.Kind == "sort" {
			for _, nx := range p.Steps[i+1:] {
				if nx.Kind != "limit" {
					add("step %d (sort): 排序之后只能接 limit,实际接了 %s", i+1, nx.Kind)
					break
				}
			}
		}
	}
	sorted := false
	for i, st := range p.Steps {
		sorted = sorted || st.Kind == "sort"
		if st.Kind == "limit" && !sorted {
			add("step %d (limit): limit 之前必须有 sort(否则取哪几行不确定)", i+1)
		}
	}
	for _, d := range p.Params {
		if d.Type.Kind == JSON {
			add("参数 %s: 不能是 json", d.Name)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return cur, nil
}

// narrow: 类型收窄。条件(的 and 的每一项)是 not(is_null(col)) → 之后这一列 NOT NULL。
// 只影响类型(输出的可空性、pick_per_key 的键),不影响语义。
func narrow(cur Schema, pred *Expr) Schema {
	var conj []*Expr
	if pred.Op == "and" {
		conj = pred.Args
	} else {
		conj = []*Expr{pred}
	}
	out := append(Schema(nil), cur...)
	for _, c := range conj {
		if c.Op == "not" && c.Args[0].Op == "is_null" && c.Args[0].Args[0].Op == "col" {
			name := c.Args[0].Args[0].Name
			for i := range out {
				if out[i].Name == name {
					out[i].Type.Nullable = false
				}
			}
		}
	}
	return out
}

func checkOrder(cur Schema, order []SortKey, at string, add func(string, ...any)) {
	for _, o := range order {
		t, ok := cur.Col(o.Col)
		if !ok {
			add("%s: 排序列 %s 不存在", at, o.Col)
		} else if t.Kind == JSON {
			add("%s: 不能按 json 列排序", at)
		}
	}
}

func (p *Pipeline) typeOf(e *Expr, cur Schema) (Type, error) {
	boolT := func(nullable bool) Type { return Type{Kind: Bool, Nullable: nullable} }
	switch e.Op {
	case "col":
		t, ok := cur.Col(e.Name)
		if !ok {
			return Type{}, fmt.Errorf("列 %s 不存在", e.Name)
		}
		return t, nil
	case "const":
		if e.Type == nil {
			return Type{}, fmt.Errorf("const 需要类型")
		}
		t := *e.Type
		t.Nullable = e.Value == nil
		return t, nil
	case "param":
		d := p.Param(e.Name)
		if d == nil {
			return Type{}, fmt.Errorf("参数 %s 没有声明", e.Name)
		}
		if d.List {
			return Type{}, fmt.Errorf("列表参数 %s 只能用在 in_list / absent", e.Name)
		}
		t := d.Type
		t.Nullable = !d.Required
		return t, nil
	case "cmp":
		if !cmpOps[e.Name] || len(e.Args) != 2 {
			return Type{}, fmt.Errorf("cmp: 运算符 %q / 参数个数不对", e.Name)
		}
		a, err := p.typeOf(e.Args[0], cur)
		if err != nil {
			return Type{}, err
		}
		b, err := p.typeOf(e.Args[1], cur)
		if err != nil {
			return Type{}, err
		}
		if !comparable(a.Kind, b.Kind) {
			return Type{}, fmt.Errorf("cmp: %s 和 %s 不能比较", a.Kind, b.Kind)
		}
		return boolT(a.Nullable || b.Nullable), nil
	case "and", "or":
		if len(e.Args) < 2 {
			return Type{}, fmt.Errorf("%s 至少两个参数", e.Op)
		}
		nullable := false
		for _, x := range e.Args {
			t, err := p.typeOf(x, cur)
			if err != nil {
				return Type{}, err
			}
			if t.Kind != Bool {
				return Type{}, fmt.Errorf("%s 的参数必须是 bool", e.Op)
			}
			nullable = nullable || t.Nullable
		}
		return boolT(nullable), nil
	case "not":
		t, err := p.typeOf(e.Args[0], cur)
		if err != nil {
			return Type{}, err
		}
		if t.Kind != Bool {
			return Type{}, fmt.Errorf("not 的参数必须是 bool")
		}
		return t, nil
	case "is_null":
		if _, err := p.typeOf(e.Args[0], cur); err != nil {
			return Type{}, err
		}
		return boolT(false), nil
	case "absent":
		if p.Param(e.Name) == nil {
			return Type{}, fmt.Errorf("参数 %s 没有声明", e.Name)
		}
		return boolT(false), nil
	case "in_list", "starts_with":
		d := p.Param(e.Name)
		if d == nil {
			return Type{}, fmt.Errorf("参数 %s 没有声明", e.Name)
		}
		a, err := p.typeOf(e.Args[0], cur)
		if err != nil {
			return Type{}, err
		}
		if e.Op == "in_list" {
			if !d.List || !comparable(a.Kind, d.Type.Kind) {
				return Type{}, fmt.Errorf("in_list: 参数 %s 必须是 %s 的列表", e.Name, a.Kind)
			}
			return boolT(a.Nullable), nil
		}
		if d.List || a.Kind != Text || d.Type.Kind != Text {
			return Type{}, fmt.Errorf("starts_with: 只能用于文本")
		}
		return boolT(a.Nullable || !d.Required), nil
	case "json_field":
		a, err := p.typeOf(e.Args[0], cur)
		if err != nil {
			return Type{}, err
		}
		if a.Kind != JSON || e.Type == nil || e.Type.Kind == JSON {
			return Type{}, fmt.Errorf("json_field: 输入必须是 json,输出必须是标量类型")
		}
		return Type{Kind: e.Type.Kind, Nullable: true}, nil
	case "tuple_after":
		if len(e.Args) == 0 || len(e.Args) != len(e.Params) {
			return Type{}, fmt.Errorf("tuple_after: 列和参数的个数必须相同")
		}
		for i, c := range e.Args {
			t, err := p.typeOf(c, cur)
			if err != nil {
				return Type{}, err
			}
			if t.Nullable {
				return Type{}, fmt.Errorf("tuple_after: 游标列 %s 必须 NOT NULL", c.Name)
			}
			d := p.Param(e.Params[i])
			if d == nil || d.List || !comparable(t.Kind, d.Type.Kind) {
				return Type{}, fmt.Errorf("tuple_after: 参数 %s 的类型不对", e.Params[i])
			}
		}
		return boolT(false), nil
	}
	return Type{}, fmt.Errorf("不认识的表达式元件 %q", e.Op)
}
