package exits

import (
	"fmt"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// 多条: 筛选 + 排序,不分页。有上限: 查询多取一行,超过上限时报错(不悄悄截断)。
func init() {
	Register(&Exit{
		Kind: "list_all", Name: "多条", Cardinality: "many", Cell: "多条", Rows: true,
		DesignKeys: append([]string{"排序", "上限", "每组取一条"}, GroupKeys...),
		ScalarKeys: []string{"上限"},
		Parse: func(d Design, p *spec.Pattern) {
			cols, desc, err := d.Order("排序")
			if err != nil {
				d.Err("%v", err)
			}
			setOrder(p, cols, desc)
			if _, err := fmt.Sscan(d.Str("上限"), &p.MaxRows); err != nil || p.MaxRows <= 0 {
				d.Err("多条: 上限 必须是正整数(超过上限时返回错误,不会悄悄截断)")
			}
			p.PickPerGroup = parsePick(d)
			p.Group, p.Rank = parseGroup(d)
		},
		Check: func(s *spec.Spec) []string {
			var errs []string
			if len(s.Pattern.OrderBy) == 0 {
				errs = append(errs, "pattern list_all: order_by is required (results must have a stable order)")
			}
			errs = append(errs, checkDirection("list_all", s.Pattern)...)
			if s.Pattern.MaxRows <= 0 {
				errs = append(errs, "pattern list_all: max_rows > 0 is required")
			}
			if s.Datasource.SQL != "" {
				errs = append(errs, "pattern list_all: 只支持声明式(不能用 datasource.sql)")
			}
			return append(errs, checkGroup("list_all", s)...)
		},
		RowsDesc: func(p spec.Pattern) string {
			return fmt.Sprintf("最多 %d 件;超过时返回错误(不会截断)", p.MaxRows)
		},
		ItemKey: lastOrder,
		IR: func(s *spec.Spec, api *ir.API) error {
			items, err := ItemsField(api, "list_all")
			if err != nil {
				return err
			}
			api.ListAll = &ir.ListAll{OrderBy: s.Pattern.OrderBy, Desc: s.Pattern.Direction == "desc", MaxRows: s.Pattern.MaxRows, ItemsField: items}
			return nil
		},
		Parts: func(s *spec.Spec, x PartsCtx, parts *cells.Parts, p *core.Pipeline) error {
			parts.Order = orderFrom(s.Pattern)
			parts.Limit = int(s.Pattern.MaxRows) + 1
			parts.Group = pickStep(s.Pattern.PickPerGroup)
			parts.Aggregate, parts.Rank = groupStep(s.Pattern.Group), rankStep(s.Pattern.Rank)
			return nil
		},
		DiffMulti: true,
		DiffCover: func(api *ir.API, tags map[string]int) string {
			out := fmt.Sprintf("、超过上限 %d 次", tags["over"])
			if tags["over"] == 0 {
				out += fmt.Sprintf("(上限 %d ≥ 测试数据量,超限的出口行为没有测到)", api.ListAll.MaxRows)
			}
			return out
		},
		DiffCase: func(x DiffCtx, rows []core.Row, req map[string]any) (spec.Case, string, string) {
			la := x.API().ListAll
			n := len(rows)
			if n > int(la.MaxRows) { // 多取的那一行: 超过上限 → 报错,不截断
				return spec.Case{Request: req, Expect: spec.Expect{Code: "FAILED_PRECONDITION", MessageContains: "too many"}},
					fmt.Sprintf("超过上限(%d 行以上)", n), "over"
			}
			msg := x.API().Response.Field(la.ItemsField).Message
			list := []any{}
			for _, r := range rows {
				list = append(list, x.Item(r, msg))
			}
			return spec.Case{Request: req, Expect: spec.Expect{Code: "OK", Response: map[string]any{la.ItemsField: list}}}, fmt.Sprintf("%d 行", n), ""
		},
	})
}
