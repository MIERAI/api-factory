package golang

// 嵌套(核心的 nest)的 handler 部分: 子行 → repeated 的子消息;子行多于上限 → FAILED_PRECONDITION。
// 查询函数(gen/db)已经把子行的 JSON 扫描成结构体(querygen.go)。

import (
	"fmt"
	"strings"

	"apifactory/factory/ir"
)

// nestLimit: 一个嵌套的列: 行的字段名(gen/db)和上限。
type nestLimit struct {
	Field string
	Name  string
	Max   int
}

// nestLimits: 流水线里的 nest 步骤 → 行里的字段(输出改名时按响应字段名)。
func nestLimits(api *ir.API) []nestLimit {
	if api.Pipeline == nil {
		return nil
	}
	var out []nestLimit
	for _, st := range api.Pipeline.Steps {
		if st.Kind != "nest" {
			continue
		}
		name := st.As
		for k, v := range api.Rename {
			if v == st.As {
				name = k
			}
		}
		out = append(out, nestLimit{Field: dbName(name), Name: name, Max: st.N})
	}
	return out
}

// nestGuard: 子行多于上限 → 整个请求 FAILED_PRECONDITION(不截断)。只看响应里的行(分页: 去掉多取的一行之后)。
// row = 一行的变量(单条);rows = 行的切片(多条 / 一览)。
func (g *genCtx) nestGuard(row, rows string) string {
	ls := nestLimits(g.api)
	if len(ls) == 0 {
		return ""
	}
	g.use(runtimeModule+"/apierr", "")
	var b strings.Builder
	check := func(v string) {
		for _, l := range ls {
			fmt.Fprintf(&b, "\tif len(%s.%s) > %d {\n\t\treturn nil, apierr.FailedPrecondition(\"too many %s (more than %d); nested rows are never truncated\")\n\t}\n", v, l.Field, l.Max, l.Name, l.Max)
		}
	}
	if row != "" {
		check(row)
		return b.String()
	}
	fmt.Fprintf(&b, "\tfor _, r := range %s {\n", rows)
	check("r")
	b.WriteString("\t}\n")
	return b.String()
}

// childrenExpr: 子行的切片(gen/db 的 Item)→ repeated 的子消息。每个字段和标量的列一样转换(resExpr)。
func (g *genCtx) childrenExpr(src, srcType string, f *ir.Field) (string, error) {
	item := strings.TrimPrefix(srcType, "[]")
	cols, ok := g.db.structs[item]
	if !ok || f.Message == nil {
		return "", fmt.Errorf("query column %s (%s) cannot be mapped to repeated message field %s", src, srcType, f.Name)
	}
	var lines []string
	for i := range f.Message.Fields {
		mf := &f.Message.Fields[i]
		col, ok := fieldByNorm(cols, mf.Name)
		if !ok {
			return "", fmt.Errorf("response field %s.%s has no matching column in %s", f.Message.Name, mf.Name, item)
		}
		e, err := g.resExpr("c."+col.Name, col.Type, mf)
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("%s: %s,", goCamelCase(mf.Name), e))
	}
	msg := g.pbType(f.Message)
	return fmt.Sprintf("func() []*%[1]s {\n\t\tout := make([]*%[1]s, 0, len(%[2]s))\n\t\tfor _, c := range %[2]s {\n\t\t\tout = append(out, &%[1]s{\n%[3]s\n})\n\t\t}\n\t\treturn out\n\t}()",
		msg, src, strings.Join(lines, "\n")), nil
}
