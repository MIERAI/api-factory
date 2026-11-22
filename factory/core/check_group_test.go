package core

import (
	"strings"
	"testing"
)

// group / rank 的设计规则检查(DRC): 编译时就拒绝的写法。
func TestCheckGroup(t *testing.T) {
	scan := Scan("c", certCols...)
	ok := []*Pipeline{
		{Steps: []Step{scan, Group([]string{"g", "s"}, Count("n"), Sum("total", "d"), Min("m", "t")), Sort(SortKey{Col: "s"}, SortKey{Col: "g"}), Limit(3)}},
		{Steps: []Step{scan, Group([]string{"g"}, Count("n")), Rank("r", SortKey{"n", true}), Project(NamedExpr{"gg", Col("g")}, NamedExpr{"r", Col("r")}), Sort(SortKey{Col: "r"}, SortKey{Col: "gg"})}},
	}
	for i, p := range ok {
		s, err := Check(p)
		if err != nil {
			t.Errorf("#%d: %v", i, err)
		}
		if i == 0 {
			if want := "g:int32 s:text? n:int64 total:decimal m:text"; schemaText(s) != want {
				t.Errorf("group 之后的列: %s, want %s", schemaText(s), want)
			}
		}
	}
	bad := map[string]*Pipeline{
		"rank 只能放在 group 之后": {Steps: []Step{scan, Rank("r", SortKey{Col: "g"}), Sort(SortKey{Col: "k"})}},
		"必须包含全部分组键":          {Steps: []Step{scan, Group([]string{"g", "s"}, Count("n")), Sort(SortKey{Col: "n"}, SortKey{Col: "g"}), Limit(3)}},
		"被投影丢掉了":             {Steps: []Step{scan, Group([]string{"g", "s"}, Count("n")), Project(NamedExpr{"g", Col("g")}, NamedExpr{"n", Col("n")}), Sort(SortKey{Col: "g"})}},
		"sum 只能用于整数 / 十进制":   {Steps: []Step{scan, Group([]string{"g"}, Sum("x", "t"))}},
		"count 不带列":          {Steps: []Step{scan, Group([]string{"g"}, Agg{Name: "x", Func: "count", Col: "i"})}},
		"不能用于 bool":          {Steps: []Step{scan, Group([]string{"g"}, Min("x", "b"))}},
		"需要至少一个分组键":          {Steps: []Step{scan, Group(nil, Count("n"))}},
		"只能有一个 group":        {Steps: []Step{scan, Group([]string{"g"}, Count("n")), Group([]string{"g"}, Count("m"))}},
		"和已有的列重名":            {Steps: []Step{scan, Group([]string{"g"}, Count("n")), Rank("n", SortKey{Col: "g"})}},
		"分组键 j 不能是 json":     {Steps: []Step{scan, Group([]string{"j"}, Count("n"))}},
		"聚合的列名 \"g\" 为空或重复":  {Steps: []Step{scan, Group([]string{"g"}, Count("g"))}},
	}
	for want, p := range bad {
		if _, err := Check(p); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error containing %q, got %v", want, err)
		}
	}
}

func schemaText(s Schema) string {
	var out []string
	for _, c := range s {
		x := c.Name + ":" + string(c.Type.Kind)
		if c.Type.Nullable {
			x += "?"
		}
		out = append(out, x)
	}
	return strings.Join(out, " ")
}
