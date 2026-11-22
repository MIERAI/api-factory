package accept

// 差分测试: 分组(group)/ 名次(rank)的覆盖条件。
//
// 和筛选的敏感度一样的做法: 每一种"错误的实现"都要在 ≥3 个合法请求里和正确的结果不同(参考求值器上判定),
// 否则换种子 / 数据加倍重来。错误的实现(设计稿 r2-relations §2.4 的变异):
//   - 少一个分组键(那一列取组里第一行的值)
//   - sum ↔ count(合计写成件数)
//   - NULL 的键分成多组(每个 NULL 一组)
//   - rank → dense_rank(不跳号)/ row_number(并列也不同名次)
// 另外的条件: 有多行的组、只有一行的组(分组的输入里);min / max 的列可以为空时,结果出现空值。
//
// 错误的实现不一定能写成核心的流水线(核心没有"每个 NULL 一组"): 先用参考求值器算到 group / rank 之前,
// 在这里按错误的方式算这一步,再把结果当作一张表(scan)接着跑后面的步骤。

import (
	"fmt"
	"sort"
	"strings"

	"apifactory/factory/core"
)

// groupIndex: API 部分的 group / rank 的位置(没有 → -1)。
func groupIndex(p *core.Pipeline, kind string) int {
	for i, st := range p.Steps {
		if st.Kind == kind {
			return i
		}
	}
	return -1
}

// evalParams: 参考求值器的参数(请求 + 出口另外的参数)。
func (e *diffEnv) evalParams(ps core.Params) core.Params {
	params := core.Params{}
	for k, v := range ps {
		params[k] = v
	}
	if e.exit.DiffParams != nil {
		for k, v := range e.exit.DiffParams(e.api) {
			params[k] = v
		}
	}
	return params
}

// stepInput: 第 idx 步的输入(前面的步骤的结果)。
func (e *diffEnv) stepInput(ps core.Params, idx int) (*core.Result, error) {
	ps = e.align(ps)
	p := e.api.Pipeline
	return core.Eval(&core.Pipeline{Steps: p.Steps[:idx], Params: p.Params}, map[string][]core.Row{e.table: e.rowsFor(ps)}, e.evalParams(ps))
}

// replaceStep: 第 idx 步换成 f(输入)的结果,后面的步骤照常 → 规范文本(和 evalCanon 一样;出错 → "error")。
func (e *diffEnv) replaceStep(ps core.Params, idx int, f func(in *core.Result) ([]core.Row, core.Schema, error)) string {
	in, err := e.stepInput(ps, idx)
	if err != nil {
		return "error"
	}
	rows, schema, err := f(in)
	if err != nil {
		return "error"
	}
	p := e.api.Pipeline
	rest := &core.Pipeline{Params: p.Params, Steps: append([]core.Step{core.Scan("__mut", schema...)}, p.Steps[idx+1:]...)}
	res, err := core.Eval(rest, map[string][]core.Row{"__mut": rows}, e.evalParams(e.align(ps)))
	if err != nil {
		return "error"
	}
	var b strings.Builder
	for _, r := range res.Rows {
		for _, c := range res.Columns {
			b.WriteString(core.Canonical(r[c.Name]) + "|")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// regroup: 按 gid(行 → 组的标识)分组,聚合用核心的 group(规则不变,只有"怎么分组"是错的)。键的值取组里第一行的。
func regroup(st core.Step, in *core.Result, gid func(i int, r core.Row) string) ([]core.Row, core.Schema, error) {
	schema := append(append(core.Schema(nil), in.Columns...), core.NamedType{Name: "__gid", Type: core.Type{Kind: core.Text}})
	rows := make([]core.Row, len(in.Rows))
	first := map[string]core.Row{}
	for i, r := range in.Rows {
		x := core.Row{}
		for k, v := range r {
			x[k] = v
		}
		g := gid(i, r)
		x["__gid"] = g
		rows[i] = x
		if _, ok := first[g]; !ok {
			first[g] = r
		}
	}
	res, err := core.Eval(&core.Pipeline{Steps: []core.Step{core.Scan("__in", schema...), core.Group([]string{"__gid"}, st.Aggs...)}},
		map[string][]core.Row{"__in": rows}, nil)
	if err != nil {
		return nil, nil, err
	}
	var out []core.Row
	for _, r := range res.Rows {
		x := core.Row{}
		for _, k := range st.Keys {
			x[k] = first[r["__gid"].(string)][k]
		}
		for _, a := range st.Aggs {
			x[a.Name] = r[a.Name]
		}
		out = append(out, x)
	}
	outSchema := core.Schema{}
	for _, k := range st.Keys {
		t, _ := in.Columns.Col(k)
		outSchema = append(outSchema, core.NamedType{Name: k, Type: t})
	}
	for _, c := range res.Columns[1:] {
		outSchema = append(outSchema, c)
	}
	return out, outSchema, nil
}

func keyText(r core.Row, keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(core.Canonical(r[k]) + "\x00")
	}
	return b.String()
}

// groupTargets: 分组 / 名次的覆盖条件(sensTarget: custom = 错误的实现的结果;cond = 请求满足的条件)。
func (e *diffEnv) groupTargets() []sensTarget {
	p := e.api.Pipeline
	gi := groupIndex(p, "group")
	if gi < 0 {
		return nil
	}
	st := p.Steps[gi]
	var out []sensTarget
	sizes := func(ps core.Params) map[string]int {
		in, err := e.stepInput(ps, gi)
		if err != nil {
			return nil
		}
		m := map[string]int{}
		for _, r := range in.Rows {
			m[keyText(r, st.Keys)]++
		}
		return m
	}
	out = append(out, sensTarget{desc: "分组: 有多行的组", cond: func(ps core.Params) bool {
		for _, n := range sizes(ps) {
			if n >= 2 {
				return true
			}
		}
		return false
	}})
	out = append(out, sensTarget{desc: "分组: 只有一行的组", cond: func(ps core.Params) bool {
		for _, n := range sizes(ps) {
			if n == 1 {
				return true
			}
		}
		return false
	}})
	// min / max 的结果出现 NULL(列可以为空时: 组里全部为空 → 不设置;写成 coalesce(…, 0) 的实现要看得出来)。
	// 看最后的输出(出口截断 / 分页之后仍然看得到)
	if schema, err := core.Check(&core.Pipeline{Steps: p.Steps[:gi], Params: p.Params}); err == nil {
		for _, a := range st.Aggs {
			if t, ok := schema.Col(a.Col); !ok || !t.Nullable || (a.Func != "min" && a.Func != "max") {
				continue
			}
			name := a.Name
			out = append(out, sensTarget{desc: "分组: " + name + " 的结果为空(组里全部为空)", cond: func(ps core.Params) bool {
				rows, err := e.Eval(ps)
				if err != nil {
					return false
				}
				for _, r := range rows {
					if v, has := r[name]; has && v == nil {
						return true
					}
				}
				return false
			}})
		}
	}
	for ki, k := range st.Keys { // 少一个分组键
		if e.fixedCols()[k] { // 请求固定了这一列(必填的等于): 少了它分组也一样
			continue
		}
		rest := append(append([]string(nil), st.Keys[:ki]...), st.Keys[ki+1:]...)
		out = append(out, sensTarget{desc: "分组: 少了分组键 " + k, custom: func(ps core.Params) string {
			return e.replaceStep(ps, gi, func(in *core.Result) ([]core.Row, core.Schema, error) {
				return regroup(st, in, func(_ int, r core.Row) string { return keyText(r, rest) })
			})
		}})
	}
	for ai, a := range st.Aggs { // 合计写成件数
		if a.Func != "sum" {
			continue
		}
		ai := ai
		out = append(out, sensTarget{desc: "分组: " + a.Name + " 的合计写成件数", custom: func(ps core.Params) string {
			return e.replaceStep(ps, gi, func(in *core.Result) ([]core.Row, core.Schema, error) {
				mut := st
				mut.Aggs = append([]core.Agg(nil), st.Aggs...)
				mut.Aggs[ai] = core.Count(a.Name)
				return regroup(mut, in, func(_ int, r core.Row) string { return keyText(r, st.Keys) })
			})
		}})
	}
	// NULL 的键分成多组: 分组的输入里键可以为空时(没有被请求固定、也没有被筛选收窄)
	if schema, err := core.Check(&core.Pipeline{Steps: p.Steps[:gi], Params: p.Params}); err == nil {
		nullable := false
		for _, k := range st.Keys {
			if t, ok := schema.Col(k); ok && t.Nullable && !e.fixedCols()[k] {
				nullable = true
			}
		}
		if nullable {
			out = append(out, sensTarget{desc: "分组: NULL 的键分成多组", custom: func(ps core.Params) string {
				return e.replaceStep(ps, gi, func(in *core.Result) ([]core.Row, core.Schema, error) {
					return regroup(st, in, func(i int, r core.Row) string {
						g := keyText(r, st.Keys)
						for _, k := range st.Keys {
							if r[k] == nil {
								return fmt.Sprintf("%s#%d", g, i)
							}
						}
						return g
					})
				})
			}})
		}
	}
	if ri := groupIndex(p, "rank"); ri >= 0 {
		rs := p.Steps[ri]
		for _, wrong := range []string{"dense_rank", "row_number"} {
			wrong := wrong
			out = append(out, sensTarget{desc: "名次: RANK 写成 " + wrong, custom: func(ps core.Params) string {
				return e.replaceStep(ps, ri, func(in *core.Result) ([]core.Row, core.Schema, error) {
					rows := make([]core.Row, len(in.Rows))
					for i, r := range in.Rows {
						x := core.Row{}
						for k, v := range r {
							x[k] = v
						}
						rows[i] = x
					}
					cmp := func(a, b core.Row) int {
						for _, o := range rs.Order {
							c := core.Compare(a[o.Col], b[o.Col])
							if o.Desc {
								c = -c
							}
							if c != 0 {
								return c
							}
						}
						return 0
					}
					sort.SliceStable(rows, func(i, j int) bool { return cmp(rows[i], rows[j]) < 0 })
					var n int64
					for i, r := range rows {
						switch {
						case wrong == "row_number":
							n = int64(i + 1)
						case i == 0 || cmp(rows[i-1], r) != 0:
							n++
						}
						r[rs.As] = n
					}
					schema := append(append(core.Schema(nil), in.Columns...), core.NamedType{Name: rs.As, Type: core.Type{Kind: core.Int64}})
					return rows, schema, nil
				})
			}})
		}
	}
	return out
}
