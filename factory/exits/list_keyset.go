package exits

import (
	"fmt"
	"regexp"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

const diffPageSize = 3

// 一览: 筛选 + keyset 分页。请求加 page_size / page_token,响应加 next_page_token。
func init() {
	Register(&Exit{
		Kind: "list_keyset", Name: "一览", Cardinality: "many", Cell: "分页", Rows: true, EmptyRequest: true, NotNullOrder: true,
		DesignKeys: []string{"排序", "分页", "每组取一条"},
		Parse: func(d Design, p *spec.Pattern) {
			cols, desc, err := d.Order("排序")
			if err != nil {
				d.Err("%v", err)
			}
			setOrder(p, cols, desc)
			if p.Direction == "mixed" { // 游标按整组比较(行比较),方向必须一致
				d.Err("一览: 排序的方向必须一致(分页的游标按整组比较);方向混在一起请用 多条 / 前N条")
			}
			p.PickPerGroup = parsePick(d) // 每组取一条之后再分页(游标在取完之后的行上)
			pg := d.Node("分页")
			if _, err := fmt.Sscan(nodeStr(pg, "默认每页"), &p.PageSize.Default); err != nil {
				d.Err("一览: 分页.默认每页 必须是整数")
			}
			if _, err := fmt.Sscan(nodeStr(pg, "每页最多"), &p.PageSize.Max); err != nil {
				d.Err("一览: 分页.每页最多 必须是整数")
			}
		},
		Check: func(s *spec.Spec) []string {
			var errs []string
			if len(s.Pattern.OrderBy) == 0 {
				errs = append(errs, "pattern list_keyset: order_by is required (last column must be unique, e.g. id)")
			}
			if s.Pattern.Direction != "asc" && s.Pattern.Direction != "desc" {
				errs = append(errs, "pattern list_keyset: direction must be asc or desc")
			}
			if ps := s.Pattern.PageSize; ps.Default <= 0 || ps.Max < ps.Default {
				errs = append(errs, "pattern list_keyset: page_size.default > 0 and max >= default are required")
			}
			if regexp.MustCompile(`(?i)\b(order\s+by|limit|offset)\b`).MatchString(s.Datasource.SQL) {
				errs = append(errs, "pattern list_keyset: sql must not contain ORDER BY / LIMIT / OFFSET (the factory adds them)")
			}
			return errs
		},
		Request: func(p spec.Pattern) []Field {
			return []Field{
				{Name: "page_size", Type: "int32", Kind: "int", Desc: fmt.Sprintf("每页件数。0 = 默认 %d,最多 %d", p.PageSize.Default, p.PageSize.Max),
					Rules: []string{fmt.Sprintf("int32 = {gte: 0, lte: %d}", p.PageSize.Max)}},
				{Name: "page_token", Type: "string", Kind: "string", Desc: "上一页返回的 next_page_token(不透明的字符串)"},
			}
		},
		Extra:   []Field{{Name: "next_page_token", Type: "string", Kind: "string", Desc: "还有下一页时不为空"}},
		ItemKey: lastOrder,
		IR: func(s *spec.Spec, api *ir.API) error {
			items, err := ItemsField(api, "list_keyset")
			if err != nil {
				return err
			}
			for _, name := range []string{"page_size", "page_token"} {
				if api.Request.Field(name) == nil {
					return fmt.Errorf("pattern list_keyset: %s must have field %s", api.Request.Name, name)
				}
			}
			if api.Response.Field("next_page_token") == nil {
				return fmt.Errorf("pattern list_keyset: %s must have field next_page_token", api.Response.Name)
			}
			api.Keyset = &ir.Keyset{OrderBy: s.Pattern.OrderBy, Desc: s.Pattern.Direction == "desc",
				DefaultSize: s.Pattern.PageSize.Default, MaxSize: s.Pattern.PageSize.Max, ItemsField: items}
			return nil
		},
		Parts: func(s *spec.Spec, x PartsCtx, parts *cells.Parts, p *core.Pipeline) error {
			var params []string
			for _, c := range s.Pattern.OrderBy {
				k, err := x.ColumnKind(c)
				if err != nil {
					return fmt.Errorf("order_by: %w", err)
				}
				p.Params = append(p.Params, core.ParamDecl{Name: "cursor_" + c, Type: core.Type{Kind: k}})
				params = append(params, "cursor_"+c)
			}
			parts.Order = orderFrom(s.Pattern)
			parts.Cursor = core.TupleAfter(s.Pattern.OrderBy, params, s.Pattern.Direction != "asc")
			parts.Rows = "row_limit"
			parts.Group = pickStep(s.Pattern.PickPerGroup)
			p.Params = append(p.Params, core.ParamDecl{Name: "row_limit", Type: core.Type{Kind: core.Int32}, Required: true})
			return nil
		},
		DiffRowsNeed: func(*ir.API) (int, int) { return 5, 1 },                                      // 出口用例(途中改 page_size)要 ≥5 行
		DiffParams:   func(*ir.API) core.Params { return core.Params{"row_limit": int64(1 << 30)} }, // 完整的有序结果,再按页切
		DiffMulti:    true,
		DiffCase: func(x DiffCtx, rows []core.Row, req map[string]any) (spec.Case, string, string) {
			k := x.API().Keyset
			msg := x.API().Response.Field(k.ItemsField).Message
			n := len(rows)
			size := []int{1, diffPageSize, diffPageSize}[x.Rand().Intn(3)] // 1: 并列行跨页
			omitSize := x.Rand().Intn(4) == 0                              // 有时不传 page_size: 用式样书的默认值
			if omitSize {
				size = int(k.DefaultSize)
			}
			var steps []spec.Step
			for start := 0; start == 0 || start < n; start += size {
				end := min(start+size, n)
				list := []any{}
				for _, r := range rows[start:end] {
					list = append(list, x.Item(r, msg))
				}
				stepReq := map[string]any{"page_size": size}
				for k, v := range req {
					stepReq[k] = v
				}
				if omitSize {
					delete(stepReq, "page_size")
				}
				if start > 0 {
					stepReq["page_token"] = "$prev.next_page_token"
				}
				token := any("$absent")
				if end < n {
					token = "$nonempty"
				}
				steps = append(steps, spec.Step{Request: stepReq, Expect: spec.Expect{Code: "OK", Response: map[string]any{k.ItemsField: list, "next_page_token": token}}})
			}
			return spec.Case{Steps: steps}, fmt.Sprintf("%d 行 / %d 页", n, len(steps)), ""
		},
		DiffExtra: pageExitCases,
	})
}

// pageExitCases: 分页出口自己的行为(不在 SQL 流水线里): 途中改 page_size / 途中改筛选条件 / 非法 token。
// 期待值同样来自参考求值器。
func pageExitCases(x DiffCtx) []Case {
	items := x.API().Keyset.ItemsField
	msg := x.API().Response.Field(items).Message
	// 找一个合法、结果 ≥ 5 行的请求做基础
	var base core.Params
	var rows []core.Row
	for _, ps := range x.Requests() {
		req := x.Request(ps)
		req["page_size"] = 1
		if bad, err := x.Invalid(req); err != nil || bad {
			continue
		}
		r, err := x.Eval(ps)
		if err == nil && len(r) >= 5 && (base == nil || len(r) > len(rows)) { // 结果最多的(默认件数的用例要能翻页)
			base, rows = ps, r
		}
	}
	if base == nil {
		return []Case{{Name: "分页出口", Fail: "找不到结果 ≥ 5 行的请求(覆盖不足)"}}
	}
	list := func(rs []core.Row) []any {
		out := []any{}
		for _, r := range rs {
			out = append(out, x.Item(r, msg))
		}
		return out
	}
	with := func(extra map[string]any) map[string]any {
		req := x.Request(base)
		for k, v := range extra {
			req[k] = v
		}
		return req
	}
	out := []Case{
		{Name: "分页出口: 途中改 page_size 是允许的", Case: spec.Case{Steps: []spec.Step{
			{Request: with(map[string]any{"page_size": 2}), Expect: spec.Expect{Code: "OK", Response: map[string]any{items: list(rows[:2]), "next_page_token": "$nonempty"}}},
			{Request: with(map[string]any{"page_size": 3, "page_token": "$prev.next_page_token"}), Expect: spec.Expect{Code: "OK", Response: map[string]any{items: list(rows[2:5])}}},
		}}},
		{Name: "分页出口: 非法 page_token → INVALID_ARGUMENT", Case: spec.Case{Request: with(map[string]any{"page_token": "not-a-valid-token"}),
			Expect: spec.Expect{Code: "INVALID_ARGUMENT", MessageContains: "page_token"}}},
	}
	// 途中改筛选条件 → 旧 token 失效: 改一个筛选参数的值,改了之后的请求本身必须合法
	changedParams := 0
	for _, d := range x.UserParams() {
		col := x.Column(d.Name)
		for _, r := range x.Anchors() {
			v := r[col]
			if v == nil || core.Canonical(v) == core.Canonical(base[d.Name]) {
				continue
			}
			nv := x.Present(col, v)
			if d.List {
				nv = []any{nv}
			}
			changed := with(map[string]any{d.Name: nv, "page_size": 1, "page_token": "$prev.next_page_token"})
			probe := with(map[string]any{d.Name: nv, "page_size": 1})
			if bad, err := x.Invalid(probe); err != nil || bad {
				continue
			}
			out = append(out, Case{Name: fmt.Sprintf("分页出口: 途中改筛选条件(%s)→ INVALID_ARGUMENT", d.Name), Case: spec.Case{Steps: []spec.Step{
				{Request: with(map[string]any{"page_size": 1}), Expect: spec.Expect{Code: "OK", Response: map[string]any{"next_page_token": "$nonempty"}}},
				{Request: changed, Expect: spec.Expect{Code: "INVALID_ARGUMENT", MessageContains: "page_token"}},
			}}})
			changedParams++
			break
		}
		if changedParams == 3 { // 每个参数各一次,最多 3 个
			break
		}
	}
	if len(x.UserParams()) > 0 && changedParams == 0 {
		out = append(out, Case{Name: "分页出口: 途中改筛选条件", Fail: "没有可以改的筛选参数"})
	}
	return append(out, defaultPageCases(x, base, rows, list, with)...)
}

// defaultPageCases: 不传 page_size / page_size = 0 → 用式样书的默认件数;不传 page_size 翻到第 2 页。
func defaultPageCases(x DiffCtx, base core.Params, rows []core.Row, list func([]core.Row) []any, with func(map[string]any) map[string]any) []Case {
	k := x.API().Keyset
	def := int(k.DefaultSize)
	first := rows[:min(def, len(rows))]
	more := len(rows) > def
	token := any("$absent")
	if more {
		token = "$nonempty"
	}
	firstPage := map[string]any{k.ItemsField: list(first), "next_page_token": token}
	out := []Case{
		{Name: "分页出口: 不传 page_size → 默认件数", Case: spec.Case{Request: with(nil), Expect: spec.Expect{Code: "OK", Response: firstPage}}},
		{Name: "分页出口: page_size = 0 → 默认件数", Case: spec.Case{Request: with(map[string]any{"page_size": 0}), Expect: spec.Expect{Code: "OK", Response: firstPage}}},
	}
	if more {
		second := rows[def:min(2*def, len(rows))]
		out = append(out, Case{Name: "分页出口: 不传 page_size 翻到第 2 页", Case: spec.Case{Steps: []spec.Step{
			{Request: with(nil), Expect: spec.Expect{Code: "OK", Response: firstPage}},
			{Request: with(map[string]any{"page_token": "$prev.next_page_token"}), Expect: spec.Expect{Code: "OK", Response: map[string]any{k.ItemsField: list(second)}}},
		}}})
	}
	return out
}
