package exits

import (
	"fmt"

	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// 单条: 按键取一行。0 行 → NOT_FOUND。
func init() {
	Register(&Exit{
		Kind: "get_by_id", Name: "按键取一条", Cardinality: "one", Cell: "单条", FromTable: true,
		DesignKeys: []string{"找不到时"},
		ScalarKeys: []string{"找不到时"},
		Parse: func(d Design, p *spec.Pattern) {
			p.NotFoundMessage = d.Str("找不到时")
			if p.NotFoundMessage == "" {
				d.Err("按键取一条: 缺少 找不到时(找不到时返回的消息)")
			}
		},
		Check: func(s *spec.Spec) []string {
			if s.Pattern.NotFoundMessage == "" {
				return []string{"pattern get_by_id: not_found_message is required"}
			}
			return nil
		},
		IR: func(s *spec.Spec, api *ir.API) error {
			api.GetByID = &ir.GetByID{NotFoundMessage: s.Pattern.NotFoundMessage}
			return nil
		},
		DiffExtra: func(x DiffCtx) []Case { // 找不到: 3 个不同的不存在的键(NOT_FOUND 的映射不能只靠一个请求)
			var out []Case
			for i := 0; i < 3; i++ {
				ps := core.Params{}
				for _, d := range x.UserParams() {
					v := x.Missing(d.Name, i)
					if v == nil {
						return out
					}
					ps[d.Name] = v
				}
				req := x.Request(ps)
				if bad, err := x.Invalid(req); err != nil || bad {
					continue
				}
				out = append(out, Case{Name: fmt.Sprintf("单条出口: 不存在的键 #%d → NOT_FOUND", i+1),
					Case: spec.Case{Request: req, Expect: spec.Expect{Code: "NOT_FOUND"}}})
			}
			return out
		},
		DiffCase: func(x DiffCtx, rows []core.Row, req map[string]any) (spec.Case, string, string) {
			api := x.API()
			switch n := len(rows); {
			case n == 0:
				return spec.Case{Request: req, Expect: spec.Expect{Code: "NOT_FOUND"}}, "", ""
			case n == 1:
				return spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: x.Item(rows[0], &api.Response)}}, "", ""
			default:
				return spec.Case{}, fmt.Sprintf("单条: 按键筛选得到 %d 行(键不唯一,设计规则的问题)", n), "error"
			}
		},
	})
}
