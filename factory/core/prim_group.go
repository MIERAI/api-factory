package core

import (
	"fmt"
	"strings"
)

// group: Keys + Aggs: 按 Keys 分组(NULL 视为同一组),每组输出一行 = 键的值 + 聚合值(agg.go)。
// 没有输入行 → 没有输出行。输出的列 = Keys(原来的类型)+ Aggs(AggType)。一条流水线只能有一个。
// 之后的 sort 必须包含全部分组键(prim_sort.go)。
// SQL: 同一层 SELECT 键, 聚合 … GROUP BY 键(sql.go 的 render / groupKeys)。

func Group(keys []string, aggs ...Agg) Step {
	return Step{Kind: "group", Keys: keys, Aggs: aggs}
}

func init() {
	registerStep(stepPrim{
		kind: "group",
		check: func(c *checker, st Step, at string) {
			if c.grouped {
				c.add("%s: 一条流水线只能有一个 group", at)
			}
			c.grouped = true
			next, err := groupSchema(c.cur, st)
			if err != nil {
				c.add("%s: %v", at, err)
			}
			c.cur = next
			c.gkeys = append([]string(nil), st.Keys...)
		},
		eval: func(e *evaluator, st Step) error {
			next, err := groupSchema(e.schema, st)
			if err != nil {
				return err
			}
			out, err := evalGroup(e.rows, e.schema, st)
			if err != nil {
				return err
			}
			e.schema, e.rows = next, out
			return nil
		},
		sql: func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error) {
			if lv.busy() {
				lv = g.wrap(lv, false)
			}
			out, err := groupSchema(lv.in, st)
			if err != nil {
				return nil, err
			}
			lv.group, lv.aggs, lv.out = st.Keys, st.Aggs, out
			return lv, nil
		},
	})
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

// evalGroup: 按键的规范文本分组(NULL = "∅",和任何文本的规范写法都不同: 文本带引号)。
// 文本按字节相等(规范文本就是字节),和 SQL 的 GROUP BY … COLLATE "C" 一致。组的顺序 = 第一次出现的顺序(无序)。
func evalGroup(rows []Row, schema Schema, st Step) ([]Row, error) {
	groups := map[string][]Row{}
	var order []string
	for _, r := range rows {
		k := keyString(r, st.Keys)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}
	var out []Row
	for _, k := range order {
		g := groups[k]
		row := Row{}
		for _, key := range st.Keys {
			row[key] = g[0][key]
		}
		for _, a := range st.Aggs {
			var col Type
			vals := make([]any, len(g))
			if a.Col != "" {
				col, _ = schema.Col(a.Col)
				for i, r := range g {
					vals[i] = r[a.Col]
				}
			}
			v, err := evalAgg(a, vals, col)
			if err != nil {
				return nil, err
			}
			row[a.Name] = v
		}
		out = append(out, row)
	}
	return out, nil
}
