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
				} else if t.Kind == JSON || t.Kind == Rows {
					add("%s: 键 %s 不能是 %s", at, k, t.Kind)
				}
				// 键可以为 NULL: NULL 自己是一组(和 SQL 的 DISTINCT ON 一样;参考求值器按规范文本分组,NULL = "∅")
			}
			checkOrder(cur, st.Order, at, add)
		case "lookup":
			cur = checkLookup(cur, st, at, add)
		case "nest":
			cur = p.checkNest(cur, st, at, add)
		case "child_agg":
			cur = p.checkChildAgg(cur, st, at, add)
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

// checkLookup: 多对一查找的 DRC。对方的键列必须覆盖对方的一个唯一键(最多一行匹配;多对一以外的关联不是 lookup),
// 两边的类型可以比较,加的列不能和已有的列重名。返回加了列之后的列(keep: 加的列可空)。
func checkLookup(cur Schema, st Step, at string, add func(string, ...any)) Schema {
	other := Schema(st.Columns)
	if st.Table == "" || len(st.Columns) == 0 {
		add("%s: 需要对方的表和列", at)
		return cur
	}
	if len(st.On) == 0 || len(st.Take) == 0 {
		add("%s: 需要关联条件(on)和加的列(take)", at)
	}
	if st.Missing != "" && st.Missing != "keep" && st.Missing != "drop" {
		add("%s: 找不到时只能是 keep / drop,实际是 %q", at, st.Missing)
	}
	keys := map[string]bool{}
	for _, k := range st.On {
		a, okA := cur.Col(k.Col)
		b, okB := other.Col(k.Key)
		switch {
		case !okA:
			add("%s: 关联的列 %s 不存在", at, k.Col)
		case !okB:
			add("%s: %s 没有列 %s", at, st.Table, k.Key)
		case !comparable(a.Kind, b.Kind):
			add("%s: 关联条件 %s = %s.%s: %s 和 %s 不能比较", at, k.Col, st.Table, k.Key, a.Kind, b.Kind)
		}
		if keys[k.Key] {
			add("%s: 对方的键列 %s 重复", at, k.Key)
		}
		keys[k.Key] = true
	}
	unique := false
	for _, u := range st.Unique {
		covered := len(u) > 0
		for _, c := range u {
			covered = covered && keys[c]
		}
		unique = unique || covered
	}
	if !unique && len(st.On) > 0 {
		var ks []string
		for _, k := range st.On {
			ks = append(ks, k.Key)
		}
		add("%s: 关联的键 %v 不是 %s 的主键或唯一约束(唯一键: %v)。多对一以外的关联不能用查找", at, ks, st.Table, st.Unique)
	}
	next := append(Schema(nil), cur...)
	seen := map[string]bool{}
	for _, c := range cur {
		seen[c.Name] = true
	}
	for _, t := range st.Take {
		ct, ok := other.Col(t.Col)
		if !ok {
			add("%s: %s 没有列 %s", at, st.Table, t.Col)
			continue
		}
		if seen[t.Name] {
			add("%s: 加的列 %s 和已有的列重名", at, t.Name)
			continue
		}
		seen[t.Name] = true
		if st.Missing != "drop" { // 找不到 → NULL
			ct.Nullable = true
		}
		next = append(next, NamedType{t.Name, ct})
	}
	return next
}

// checkChild: nest / child_agg 共同的部分: 子表、关联条件(当前行的列 = 子表的列)、子表上的条件。
func (p *Pipeline) checkChild(cur Schema, st Step, at string, add func(string, ...any)) (child Schema, ok bool) {
	child = Schema(st.Columns)
	if st.Table == "" || len(st.Columns) == 0 {
		add("%s: 需要子表和它的列", at)
		return child, false
	}
	if len(st.On) == 0 {
		add("%s: 需要关联条件(on)", at)
	}
	seen := map[string]bool{}
	for _, k := range st.On {
		a, okA := cur.Col(k.Col)
		b, okB := child.Col(k.Key)
		switch {
		case !okA:
			add("%s: 关联的列 %s 不存在", at, k.Col)
		case !okB:
			add("%s: %s 没有列 %s", at, st.Table, k.Key)
		case !comparable(a.Kind, b.Kind):
			add("%s: 关联条件 %s = %s.%s: %s 和 %s 不能比较", at, k.Col, st.Table, k.Key, a.Kind, b.Kind)
		}
		if seen[k.Key] {
			add("%s: 子表的列 %s 重复", at, k.Key)
		}
		seen[k.Key] = true
	}
	if st.Pred != nil {
		t, err := p.typeOf(st.Pred, child)
		if err != nil {
			add("%s: 子表的条件: %v", at, err)
		} else if t.Kind != Bool {
			add("%s: 子表的条件必须是 bool,实际是 %s", at, t.Kind)
		}
	}
	return child, true
}

// checkNest: 一对多嵌套的 DRC。子行的顺序必须是全序: Order 的列 + 关联的子表的列(同一个父行里是常量)
// 覆盖子表的一个唯一键,而且这个唯一键的列都不为空(可以为空的唯一列里 NULL 可以重复)。只有一层(子表的列不会是 rows)。
func (p *Pipeline) checkNest(cur Schema, st Step, at string, add func(string, ...any)) Schema {
	child, ok := p.checkChild(cur, st, at, add)
	if !ok {
		return cur
	}
	if st.N <= 0 {
		add("%s: 子行的上限 N 必须 > 0", at)
	}
	if st.As == "" {
		add("%s: 需要子行的列表的列名(as)", at)
	} else if _, dup := cur.Col(st.As); dup {
		add("%s: 列 %s 和已有的列重名", at, st.As)
	}
	if len(st.Order) == 0 {
		add("%s: 需要子行的顺序(order)", at)
	}
	checkOrder(child, st.Order, at, add)
	inOrder, inOn := map[string]bool{}, map[string]bool{}
	for _, o := range st.Order {
		inOrder[o.Col] = true
	}
	for _, k := range st.On {
		inOn[k.Key] = true
	}
	total := false
	for _, u := range st.Unique {
		all := len(u) > 0
		for _, c := range u {
			t, ok := child.Col(c)
			// 关联的列: 匹配到的子行里 = 父行的值(不为空的常量);顺序的列: 必须不为空
			all = all && ok && (inOn[c] || (inOrder[c] && !t.Nullable))
		}
		total = total || all
	}
	if !total && len(st.Order) > 0 {
		add("%s: 子行的顺序 %v 不是全序: 顺序的列(加上关联的列)要包含 %s 的主键或唯一键(不在关联条件里的列要不为空)(唯一键: %v)", at, orderNames(st.Order), st.Table, st.Unique)
	}
	if len(st.Take) == 0 {
		add("%s: 需要子行的列(take)", at)
	}
	var fields Schema
	names := map[string]bool{}
	for _, t := range st.Take {
		ct, ok := child.Col(t.Col)
		switch {
		case !ok:
			add("%s: %s 没有列 %s", at, st.Table, t.Col)
			continue
		case ct.Kind == JSON || ct.Kind == Rows:
			add("%s: 子行的列 %s 不能是 %s", at, t.Name, ct.Kind)
			continue
		case names[t.Name]:
			add("%s: 子行的列 %s 重复", at, t.Name)
			continue
		}
		names[t.Name] = true
		fields = append(fields, NamedType{t.Name, ct})
	}
	return append(append(Schema(nil), cur...), NamedType{st.As, Type{Kind: Rows, Fields: fields}})
}

// checkChildAgg: 按父行聚合的 DRC。聚合的类型来自 agg.go(集合可能为空: min / max 可空)。
func (p *Pipeline) checkChildAgg(cur Schema, st Step, at string, add func(string, ...any)) Schema {
	child, ok := p.checkChild(cur, st, at, add)
	if !ok {
		return cur
	}
	if len(st.Aggs) == 0 {
		add("%s: 需要聚合(aggs)", at)
	}
	next := append(Schema(nil), cur...)
	for _, a := range st.Aggs {
		if _, dup := next.Col(a.Name); dup || a.Name == "" {
			add("%s: 聚合的列名 %q 为空或和已有的列重名", at, a.Name)
			continue
		}
		var col *Type
		if a.Col != "" {
			t, ok := child.Col(a.Col)
			if !ok {
				add("%s: 聚合 %s: %s 没有列 %s", at, a.Name, st.Table, a.Col)
				continue
			}
			col = &t
		}
		t, err := AggType(a.Func, col, true)
		if err != nil {
			add("%s: 聚合 %s: %v", at, a.Name, err)
			continue
		}
		next = append(next, NamedType{a.Name, t})
	}
	return next
}

func orderNames(o []SortKey) []string {
	var out []string
	for _, k := range o {
		out = append(out, k.Col)
	}
	return out
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
		} else if t.Kind == JSON || t.Kind == Rows {
			add("%s: 不能按 %s 列排序(%s)", at, t.Kind, o.Col)
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
