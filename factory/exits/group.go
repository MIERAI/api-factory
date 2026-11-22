package exits

// 分组 / 排名(多条 / 前N条 / 一览 共用的设计稿键): 设计稿 → spec → 核心的 group / rank。
// 聚合的语义(类型、NULL、SQL、参考实现)只在 factory/core/agg.go。这里只有设计语言的写法。
//
//	分组: { 键: [store_code], 计算: { total: 合计(amount), lines: 件数, first: 最小(sales_date) } }
//	排名: { 排序: [total 大到小], 输出: rank }

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/core"
	"apifactory/factory/spec"
)

// GroupKeys: 支持分组的出口在设计稿里另外可以写的键。
var GroupKeys = []string{"分组", "排名"}

// AggWords: 设计语言里的聚合 → 核心的聚合(count 不带列)。
var AggWords = []struct{ Word, Func, Desc string }{
	{"件数", "count", "行数(int64;组里至少一行,不会是 0)"},
	{"合计", "sum", "合计(列): 整数 → 整数64(溢出时查询失败),十进制 → 十进制(精确);空值不算,全部为空 → 0"},
	{"最小", "min", "最小(列): 列的类型;空值不算,全部为空 → 不设置;文本按字节比较"},
	{"最大", "max", "最大(列): 同上"},
}

var reAgg = regexp.MustCompile(`^\s*(\S+?)\s*(?:[(（]\s*([a-z_][a-z0-9_]*)\s*[)）])?\s*$`)

// parseGroup: 设计稿的 分组 / 排名(没写 → nil)。
func parseGroup(d Design) (*spec.GroupBy, *spec.RankBy) {
	var g *spec.GroupBy
	if n := d.Node("分组"); n != nil {
		g = &spec.GroupBy{Keys: nodeList(n, "键")}
		checkNodeKeys(d, n, "分组", "键", "计算")
		if len(g.Keys) == 0 {
			d.Err("分组: 需要 键(按哪些列分组,例: 键: [store_code])")
		}
		calc := nodeChild(n, "计算")
		if calc != nil && calc.Kind != yaml.MappingNode {
			d.Err("分组.计算: 写成 { 输出名: 合计(列), 输出名: 件数 }")
			calc = nil
		}
		if calc != nil {
			for i := 0; i+1 < len(calc.Content); i += 2 {
				name, expr := calc.Content[i].Value, strings.TrimSpace(calc.Content[i+1].Value)
				a, err := parseAgg(name, expr)
				if err != nil {
					d.Err("分组.计算.%s: %v", name, err)
					continue
				}
				g.Aggs = append(g.Aggs, a)
			}
		}
	}
	var r *spec.RankBy
	if n := d.Node("排名"); n != nil {
		checkNodeKeys(d, n, "排名", "排序", "输出")
		r = &spec.RankBy{As: nodeStr(n, "输出")}
		cols, desc, err := d.OrderList(nodeList(n, "排序"))
		if err != nil {
			d.Err("排名.排序: %v", err)
		}
		for i, c := range cols {
			r.Order = append(r.Order, c+" "+direction(desc[i]))
		}
		if r.As == "" {
			d.Err("排名: 需要 输出(名次的列名,例: 输出: rank)")
		}
		if g == nil {
			d.Err("排名 只能和 分组 一起写(名次是对分组之后的行算的)")
		}
	}
	return g, r
}

func parseAgg(name, expr string) (spec.Agg, error) {
	m := reAgg.FindStringSubmatch(expr)
	if m == nil {
		return spec.Agg{}, fmt.Errorf("%q 写成 件数 / 合计(列) / 最小(列) / 最大(列)", expr)
	}
	for _, w := range AggWords {
		if w.Word != m[1] {
			continue
		}
		if w.Func == "count" && m[2] != "" {
			return spec.Agg{}, fmt.Errorf("件数 不带列(数的是行数)")
		}
		if w.Func != "count" && m[2] == "" {
			return spec.Agg{}, fmt.Errorf("%s 需要列: %s(列)", w.Word, w.Word)
		}
		return spec.Agg{Name: name, Func: w.Func, Column: m[2]}, nil
	}
	return spec.Agg{}, fmt.Errorf("%q 写成 件数 / 合计(列) / 最小(列) / 最大(列)", expr)
}

// checkGroup: 规格检查(spec 层)。
func checkGroup(kind string, s *spec.Spec) []string {
	g, r := s.Pattern.Group, s.Pattern.Rank
	var errs []string
	if r != nil && g == nil {
		errs = append(errs, "pattern "+kind+": rank needs group")
	}
	if g == nil {
		return errs
	}
	if s.Datasource.SQL != "" {
		errs = append(errs, "pattern "+kind+": group 只支持声明式(不能用 datasource.sql)")
	}
	if len(s.Datasource.Rename) > 0 {
		errs = append(errs, "pattern "+kind+": group 之后不能输出改名(输出的名字 = 分组键 / 聚合 / 名次)")
	}
	if len(g.Keys) == 0 {
		errs = append(errs, "pattern "+kind+": group.keys is required")
	}
	for _, a := range g.Aggs {
		if (a.Func == "count") != (a.Column == "") {
			errs = append(errs, fmt.Sprintf("pattern %s: group.aggs.%s: count 不带列,sum / min / max 要带列", kind, a.Name))
		}
	}
	if r != nil && (r.As == "" || len(r.Order) == 0) {
		errs = append(errs, "pattern "+kind+": rank.order and rank.as are required")
	}
	return errs
}

// groupStep / rankStep: spec → 核心的 group / rank(没写 → nil)。
func groupStep(g *spec.GroupBy) *core.Step {
	if g == nil {
		return nil
	}
	st := core.Group(g.Keys)
	for _, a := range g.Aggs {
		st.Aggs = append(st.Aggs, core.Agg{Name: a.Name, Func: a.Func, Col: a.Column})
	}
	return &st
}

func rankStep(r *spec.RankBy) *core.Step {
	if r == nil {
		return nil
	}
	var order []core.SortKey
	for _, o := range r.Order {
		col, dir, _ := strings.Cut(o, " ")
		order = append(order, core.SortKey{Col: col, Desc: dir == "desc"})
	}
	st := core.Rank(r.As, order...)
	return &st
}

func nodeChild(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// checkNodeKeys: 只允许已知的键(值里的半角逗号会把值切断,变成一个新的键: 一律报错)。
func checkNodeKeys(d Design, n *yaml.Node, where string, allowed ...string) {
	if n == nil || n.Kind != yaml.MappingNode {
		d.Err("%s: 写成 { %s }", where, strings.Join(allowed, ": …, ")+": …")
		return
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i].Value; !ok[k] {
			d.Err("%s: 不认识的键 %q(可以用: %s)", where, k, strings.Join(allowed, " / "))
		}
	}
}
