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
	c := &checker{p: p, add: add}
	for i, st := range p.Steps {
		c.i = i
		at := fmt.Sprintf("step %d (%s)", i+1, st.Kind)
		if st.From != "" {
			at += " [" + st.From + "]"
		}
		if i == 0 && st.Kind != "scan" {
			add("%s: 流水线必须以 scan 开始", at)
			break
		}
		sp := stepPrims[st.Kind] // 每个元件的检查在 prim_<kind>.go
		if sp == nil {
			add("%s: 不认识的元件", at)
			continue
		}
		sp.check(c, st, at)
	}
	// 跨步骤的规则(流水线的形状)
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
	return c.cur, nil
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
	if xp := exprPrims[e.Op]; xp != nil && xp.nullRejects != nil { // 每个元件的规则在 expr_<op>.go
		return xp.nullRejects(p, e)
	}
	return map[string]bool{}, false
}

func orderNames(o []SortKey) []string {
	var out []string
	for _, k := range o {
		out = append(out, k.Col)
	}
	return out
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
	if xp := exprPrims[e.Op]; xp != nil && xp.typeOf != nil { // 每个元件的类型规则在 expr_<op>.go
		return xp.typeOf(p, e, cur)
	}
	return Type{}, fmt.Errorf("不认识的表达式元件 %q", e.Op)
}

// refersRequest: 表达式里有没有请求参数 / now(来源不能依赖请求)。
func refersRequest(e *Expr) bool {
	if e == nil {
		return false
	}
	if xp := exprPrims[e.Op]; xp != nil && xp.request { // exprPrim.request
		return true
	}
	for _, a := range e.Args {
		if refersRequest(a) {
			return true
		}
	}
	return false
}
