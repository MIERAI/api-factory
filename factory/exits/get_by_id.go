package exits

import (
	"fmt"

	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// 单条: 按键取一行。0 行 → NOT_FOUND;或者 找不到时: 返回空 → OK + found = false(响应多一个 found)。
const notFoundEmpty = "返回空"

// FoundField: 找不到时: 返回空 的响应里多出来的字段。
const FoundField = "found"

func init() {
	Register(&Exit{
		Kind: "get_by_id", Name: "按键取一条", Cardinality: "one", Cell: "单条", FromTable: true,
		DesignKeys: []string{"找不到时"},
		ScalarKeys: []string{"找不到时"},
		Parse: func(d Design, p *spec.Pattern) {
			if d.Str("找不到时") == notFoundEmpty {
				p.NotFoundEmpty = true
				return
			}
			p.NotFoundMessage = d.Str("找不到时")
			if p.NotFoundMessage == "" {
				d.Err("按键取一条: 缺少 找不到时(找不到时返回的消息,或者写 %s: 响应多一个 %s,找不到时为 false)", notFoundEmpty, FoundField)
			}
		},
		Check: func(s *spec.Spec) []string {
			if s.Pattern.NotFoundMessage == "" && !s.Pattern.NotFoundEmpty {
				return []string{"pattern get_by_id: not_found_message (or not_found_empty) is required"}
			}
			return nil
		},
		ExtraFor: func(p spec.Pattern) []Field {
			if !p.NotFoundEmpty {
				return nil
			}
			return []Field{{Name: FoundField, Type: "bool", Kind: "bool", Desc: "找到了: true。找不到时 false(其他字段都不设置)"}}
		},
		IR: func(s *spec.Spec, api *ir.API) error {
			api.GetByID = &ir.GetByID{NotFoundMessage: s.Pattern.NotFoundMessage, NotFoundEmpty: s.Pattern.NotFoundEmpty}
			if s.Pattern.NotFoundEmpty && api.Response.Field(FoundField) == nil {
				return fmt.Errorf("pattern get_by_id: not_found_empty needs response field %s", FoundField)
			}
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
				if x.API().GetByID.NotFoundEmpty {
					out = append(out, Case{Name: fmt.Sprintf("单条出口: 不存在的键 #%d → OK + found = false", i+1),
						Case: spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: emptyResponse(x.API())}}})
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
			case n == 0 && api.GetByID.NotFoundEmpty:
				return spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: emptyResponse(api)}}, "", ""
			case n == 0:
				return spec.Case{Request: req, Expect: spec.Expect{Code: "NOT_FOUND"}}, "", ""
			case n == 1:
				resp := x.Item(rows[0], &api.Response)
				if api.GetByID.NotFoundEmpty {
					resp[FoundField] = true
				}
				return spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: resp}}, "", ""
			default:
				return spec.Case{}, fmt.Sprintf("单条: 按键筛选得到 %d 行(键不唯一,设计规则的问题)", n), "error"
			}
		},
	})
}

// emptyResponse: 找不到时的期待值: found = false,其他字段都不设置(全部不出现在 JSON 里)。
func emptyResponse(api *ir.API) map[string]any {
	out := map[string]any{} // false / 零值在 JSON 里不出现
	for _, f := range api.Response.Fields {
		out[f.Name] = "$absent"
	}
	return out
}
