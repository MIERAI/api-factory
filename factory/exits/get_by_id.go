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
