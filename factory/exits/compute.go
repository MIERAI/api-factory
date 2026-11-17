package exits

import (
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// 计算: 工厂负责取数据,AI 写"输入行 → 输出"的纯函数。响应的字段来自设计稿的 输出。
// 自定义: AI 写整个 handler(工厂只生成骨架)。
func init() {
	Register(&Exit{
		Kind: "compute", Name: "计算", Cardinality: "many", Cell: "计算", AI: true,
		DesignKeys: []string{"使用的列"},
		IR: func(s *spec.Spec, api *ir.API) error {
			c := &ir.Compute{LogicFunction: lowerFirst(api.Method) + "Logic"}
			for _, e := range s.LogicExamples {
				c.Examples = append(c.Examples, ir.Example{Name: e.Name, Request: e.Request, Rows: e.Rows, Expect: e.Expect})
			}
			api.Compute = c
			return nil
		},
	})
	Register(&Exit{Kind: "custom", Name: "自定义", Cardinality: "many", AI: true})
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]+'a'-'A') + s[1:]
}
