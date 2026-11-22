package exits

import (
	"fmt"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// 前N条: 筛选 + 排序,取前 N 条(不分页,不报错: "前 N 名"本来就是要截断)。
func init() {
	Register(&Exit{
		Kind: "top_n", Name: "前N条", Cardinality: "many", Cell: "多条", Rows: true,
		DesignKeys: []string{"排序", "条数"},
		ScalarKeys: []string{"条数"},
		Parse: func(d Design, p *spec.Pattern) {
			cols, desc, err := d.Order("排序")
			if err != nil {
				d.Err("%v", err)
			}
			setOrder(p, cols, desc)
			var n int
			if _, err := fmt.Sscan(d.Str("条数"), &n); err != nil || n <= 0 {
				d.Err("前N条: 条数 必须是正整数")
			}
			p.Params = map[string]any{"n": n}
		},
		Check: func(s *spec.Spec) []string {
			var errs []string
			if len(s.Pattern.OrderBy) == 0 {
				errs = append(errs, "pattern top_n: order_by is required(前 N 条必须有确定的顺序)")
			}
			errs = append(errs, checkDirection("top_n", s.Pattern)...)
			if topN(s.Pattern.Params) <= 0 {
				errs = append(errs, "pattern top_n: params.n > 0 is required")
			}
			return errs
		},
		RowsDesc: func(p spec.Pattern) string {
			return fmt.Sprintf("按排序取前 %d 件(不足时全部)", topN(p.Params))
		},
		ItemKey: lastOrder,
		IR: func(s *spec.Spec, api *ir.API) error {
			items, err := ItemsField(api, "top_n")
			if err != nil {
				return err
			}
			api.ExitParams = map[string]any{"n": topN(s.Pattern.Params), "items_field": items}
			return nil
		},
		Parts: func(s *spec.Spec, x PartsCtx, parts *cells.Parts, p *core.Pipeline) error {
			parts.Order = orderFrom(s.Pattern)
			parts.Limit = topN(s.Pattern.Params)
			return nil
		},
		DiffMulti: true,
		DiffRowsNeed: func(api *ir.API) (int, int) { // 截断要真的发生: 结果超过 N 行的请求 ≥3
			switch n := api.ExitParams["n"].(type) {
			case float64:
				return int(n) + 1, 3
			case int:
				return n + 1, 3
			}
			return 1, 3
		},
		DiffCase: func(x DiffCtx, rows []core.Row, req map[string]any) (spec.Case, string, string) {
			items := x.API().ExitParams["items_field"].(string)
			if nestOver(rows) {
				return nestOverCase(req)
			}
			msg := x.API().Response.Field(items).Message
			list := []any{}
			for _, r := range rows { // 参考求值器已经取了前 N 条
				list = append(list, x.Item(r, msg))
			}
			return spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: map[string]any{items: list}}}, fmt.Sprintf("%d 行", len(rows)), ""
		},
	})
}

// topN: params.n(YAML 里是 int,IR 的 JSON 读回来是 float64)
func topN(params map[string]any) int {
	switch v := params["n"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}
