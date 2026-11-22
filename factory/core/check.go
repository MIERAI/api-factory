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
	// gkeys: group 的键在当前位置的列名(跟着 project 的改名;被 project 丢掉 → "")。nil = 还没有 group
	var gkeys []string
	grouped := false
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
			cur = narrow(cur, st.Pred, p)
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
			for gi, k := range gkeys {
				nk := ""
				for _, x := range st.Project {
					if k != "" && x.Expr.Op == "col" && x.Expr.Name == k {
						nk = x.Name
						break
					}
				}
				gkeys[gi] = nk
			}
			cur = next
		case "group":
			if grouped {
				add("%s: 一条流水线只能有一个 group", at)
			}
			grouped = true
			next, err := groupSchema(cur, st)
			if err != nil {
				add("%s: %v", at, err)
			}
			cur = next
			gkeys = append([]string(nil), st.Keys...)
		case "rank":
			if !grouped {
				add("%s: rank 只能放在 group 之后", at)
			}
			if len(st.Order) == 0 {
				add("%s: 需要 order", at)
			}
			checkOrder(cur, st.Order, at, add)
			if st.As == "" {
				add("%s: 需要名次的列名(as)", at)
			} else if _, dup := cur.Col(st.As); dup {
				add("%s: 名次的列名 %s 和已有的列重名", at, st.As)
			}
			cur = append(append(Schema(nil), cur...), NamedType{st.As, Type{Kind: Int64}})
		case "pick_per_key":
			if len(st.Keys) == 0 || len(st.Order) == 0 {
				add("%s: 需要 keys 和 order", at)
			}
			for _, k := range st.Keys {
				t, ok := cur.Col(k)
				if !ok {
					add("%s: 键 %s 不存在", at, k)
				} else if t.Kind == JSON {
					add("%s: 键 %s 不能是 json", at, k)
				}
				// 键可以为 NULL: NULL 自己是一组(和 SQL 的 DISTINCT ON 一样;参考求值器按规范文本分组,NULL = "∅")
			}
			checkOrder(cur, st.Order, at, add)
		case "sort":
			checkOrder(cur, st.Order, at, add)
			// 分组之后的行由分组键唯一确定: 排序必须包含全部分组键,顺序才是全序(前N条 / 分页都依赖它)
			if gkeys != nil {
				in := map[string]bool{}
				for _, o := range st.Order {
					in[o.Col] = true
				}
				for gi, k := range gkeys {
					if k == "" {
						add("%s: 分组键 #%d 被投影丢掉了: 分组之后的排序必须包含全部分组键", at, gi+1)
					} else if !in[k] {
						add("%s: 分组之后的排序必须包含全部分组键(缺 %s;否则不同的组可能并列,顺序不确定)", at, k)
					}
				}
			}
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

// narrow: 类型收窄。条件在某一列为 NULL 时一定不是 TRUE(这一列"拒绝 NULL")→ 之后这一列 NOT NULL。
// 只影响类型(输出的可空性、pick_per_key 的键),不影响语义。
// 例: not(is_null(c))、cmp(=, c, p)、in_list(c, p)、必填参数的 or(absent(p), cmp(=, c, p))(absent 恒为 FALSE)。
func narrow(cur Schema, pred *Expr, p *Pipeline) Schema {
	rej, all := nullRejects(pred, p)
	out := append(Schema(nil), cur...)
	for i := range out {
		if all || rej[out[i].Name] {
			out[i].Type.Nullable = false
		}
	}
	return out
}

// nullRejects: 哪些列为 NULL 时这个条件一定不是 TRUE。all = 条件恒不为 TRUE(例: 必填参数的 absent)。保守: 不确定 → 不算。
func nullRejects(e *Expr, p *Pipeline) (cols map[string]bool, all bool) {
	cols = map[string]bool{}
	col := func(x *Expr) {
		if x != nil && x.Op == "col" {
			cols[x.Name] = true
		}
	}
	switch e.Op {
	case "cmp": // 任一边 NULL → UNKNOWN
		col(e.Args[0])
		col(e.Args[1])
	case "in_list", "starts_with": // 第一个参数 NULL → UNKNOWN
		col(e.Args[0])
	case "in_tuples": // 任一列 NULL → FALSE
		for _, a := range e.Args {
			col(a)
		}
	case "not":
		if a := e.Args[0]; a.Op == "is_null" {
			col(a.Args[0])
		}
	case "absent": // 必填参数: 恒为 FALSE
		if d := p.Param(e.Name); d != nil && d.Required {
			return cols, true
		}
	case "const":
		if b, ok := e.Value.(bool); ok && !b {
			return cols, true
		}
	case "and": // 任何一项拒绝 → 整体拒绝
		for _, a := range e.Args {
			c, al := nullRejects(a, p)
			if al {
				return cols, true
			}
			for k := range c {
				cols[k] = true
			}
		}
	case "or": // 每一项都拒绝 → 整体拒绝
		first := true
		for _, a := range e.Args {
			c, al := nullRejects(a, p)
			if al {
				continue
			}
			if first {
				cols, first = c, false
				continue
			}
			for k := range cols {
				if !c[k] {
					delete(cols, k)
				}
			}
		}
		if first { // 每一项都恒不为 TRUE
			return cols, true
		}
	}
	return cols, false
}

// groupSchema: group 之后的列 = 键(原来的类型)+ 聚合(AggType)。
func groupSchema(cur Schema, st Step) (Schema, error) {
	var out Schema
	var errs []string
	if len(st.Keys) == 0 {
		errs = append(errs, "需要至少一个分组键(没有键的合计: 没有输入行时 SQL 也会输出一行,和 group 的语义不同)")
	}
	seen := map[string]bool{}
	for _, k := range st.Keys {
		t, ok := cur.Col(k)
		switch {
		case !ok:
			errs = append(errs, fmt.Sprintf("分组键 %s 不存在", k))
			continue
		case t.Kind == JSON:
			errs = append(errs, fmt.Sprintf("分组键 %s 不能是 json", k))
		case seen[k]:
			errs = append(errs, fmt.Sprintf("分组键 %s 重复", k))
		}
		seen[k] = true
		out = append(out, NamedType{k, t}) // 键可以为 NULL: NULL 自己是一组
	}
	for _, a := range st.Aggs {
		if a.Name == "" || seen[a.Name] {
			errs = append(errs, fmt.Sprintf("聚合的列名 %q 为空或重复", a.Name))
		}
		seen[a.Name] = true
		var col *Type
		if a.Col != "" {
			t, ok := cur.Col(a.Col)
			if !ok {
				errs = append(errs, fmt.Sprintf("聚合 %s: 列 %s 不存在", a.Name, a.Col))
				continue
			}
			col = &t
		}
		t, err := AggType(a.Func, col, false)
		if err != nil {
			errs = append(errs, fmt.Sprintf("聚合 %s: %v", a.Name, err))
			continue
		}
		out = append(out, NamedType{a.Name, t})
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
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

// TypeOf: 表达式在 schema 上的类型(差分测试判断条件能不能是 UNKNOWN 用)。
func (p *Pipeline) TypeOf(e *Expr, cur Schema) (Type, error) { return p.typeOf(e, cur) }

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
	case "in_tuples":
		if len(e.Args) < 2 || len(e.Args) != len(e.Params) {
			return Type{}, fmt.Errorf("in_tuples: 至少两列,列和参数的个数必须相同")
		}
		for i, c := range e.Args {
			t, err := p.typeOf(c, cur)
			if err != nil {
				return Type{}, err
			}
			d := p.Param(e.Params[i])
			if d == nil || !d.List || !comparable(t.Kind, d.Type.Kind) {
				return Type{}, fmt.Errorf("in_tuples: 参数 %s 必须是 %s 的列表", e.Params[i], t.Kind)
			}
		}
		return boolT(false), nil
	}
	return Type{}, fmt.Errorf("不认识的表达式元件 %q", e.Op)
}
