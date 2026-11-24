package sources

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

// JSON版本: 追加型的物理表(每次变更追加一行,字段在 JSON 列里,操作列区分 登记 / 删除,按登记顺序后面的赢)。
// 逻辑表 = 每个自然键最新登记的一行;最新的是删除 → 不存在;逻辑删除 → 不存在(不回退到旧版本)。
//
//	原始表: product_events            # 物理表
//	存储列: { 数据: data, 操作: action_type, 删除值: 1, 登记顺序: [registered_at, id] }   # 可省略,这是默认值
//	逻辑删除: deleted = 1              # 可省略
//	当前状态表: 否                      # 可省略(默认 是): 每键最新的物化表 factory_current_<逻辑表>,读最新从它读
//	字段:                              # 逻辑表的列
//	  product_id: { 类型: 整数32, 说明: 商品ID }
//	  valid_from: { 类型: 整数32, 键: from, 说明: 生效开始日 }   # 键 = JSON 里的名字(默认 = 列名)
//	  line_code:  { 类型: 文本, 可以为空: 是, 说明: … }          # 主键的列也可以为空: 空值自己是一组(和原来的 DISTINCT ON 一样)
//	  start_ymd:  { 类型: 整数32, 编码: YYYYMMDD, 说明: … }     # 整数里的日期(和普通的表的列的 编码 一样)
//	  登记时间:   { 类型: 时间, 列: registered_at }               # 外壳列: 物理表的列(这个版本的值),不在 JSON 里
func init() {
	Register(&Source{
		Name: "JSON版本", Kind: "json_versions",
		DesignKeys: []string{"原始表", "存储列", "逻辑删除", "字段", "当前状态表"},
		FieldKeys:  FieldKeys,
		Parse:      parseJSONVersions,
		Columns: func(st *spec.Storage, nk []string) []Column {
			var out []Column
			for _, f := range st.Fields {
				out = append(out, Column{Name: f.Name, SQLType: f.Type, NotNull: contains(nk, f.Name) && !f.Nullable})
			}
			return out
		},
		Steps: jsonVersionsSteps,
		Row:   jsonVersionsRow,
		ShardKey: func(st *ir.Storage, column string, physical map[string]any) any {
			key := ""
			for _, f := range st.Fields {
				if f.Name == column {
					key = f.Key
				}
			}
			var obj map[string]any
			if s, ok := physical[st.DataColumn].(string); ok && json.Unmarshal([]byte(s), &obj) == nil {
				return obj[key]
			}
			return nil
		},
	})
}

// FieldKeys: JSON版本 的表的一个字段可以写的键。
var FieldKeys = []string{"类型", "键", "列", "说明", "枚举", "可以为空", "编码"}

func parseJSONVersions(d Domain) *spec.Storage {
	n, name := d.Node(), d.Name()
	st := &spec.Storage{Kind: "json_versions", Table: str(n, "原始表"), DataColumn: "data", ActionColumn: "action_type", DeleteValue: 1, OrderBy: []string{"registered_at", "id"}}
	if st.Table == "" {
		d.Err(n.Line, "表 %s: 存储: JSON版本 需要 原始表(物理表名)", name)
	}
	if st.Table == name {
		d.Err(n.Line, "表 %s: 原始表 不能和逻辑表同名(两个名字都要在 catalog 里: 原始表给 scan,逻辑表给 API)", name)
	}
	if sc := get(n, "存储列"); sc != nil {
		d.CheckKeys(sc, "表 "+name+" 的存储列", "数据", "操作", "删除值", "登记顺序")
		if v := str(sc, "数据"); v != "" {
			st.DataColumn = v
		}
		if v := str(sc, "操作"); v != "" {
			st.ActionColumn = v
		}
		if v := str(sc, "删除值"); v != "" {
			if _, err := fmt.Sscan(v, &st.DeleteValue); err != nil {
				d.Err(sc.Line, "表 %s: 删除值 必须是整数", name)
			}
		}
		if v := list(get(sc, "登记顺序")); len(v) > 0 {
			st.OrderBy = v
		}
	}
	switch str(n, "当前状态表") {
	case "", "是":
		st.State = StateTable(name)
	case "否":
	default:
		d.Err(n.Line, "表 %s: 当前状态表 只能是 是 / 否", name)
	}
	if ld := str(n, "逻辑删除"); ld != "" {
		k, v, ok := strings.Cut(ld, "=")
		if !ok {
			d.Err(n.Line, "表 %s: 逻辑删除 写成「键 = 值」(例如 deleted = 1)", name)
		} else {
			st.LogicalDelete = &spec.LogicalDelete{Key: strings.TrimSpace(k), Value: strings.TrimSpace(v)}
		}
	}
	fields := get(n, "字段")
	if fields != nil {
		for i := 0; i+1 < len(fields.Content); i += 2 {
			key, val := fields.Content[i], fields.Content[i+1]
			d.CheckKeys(val, "表 "+name+" 的字段 "+key.Value, FieldKeys...)
			nullable := false
			switch str(val, "可以为空") {
			case "是":
				nullable = true
			case "", "否":
			default:
				d.Err(key.Line, "表 %s 的字段 %s: 可以为空 只能是 是 / 否", name, key.Value)
			}
			typ := str(val, "类型")
			sqlType, ok := "", false
			if k, found := kinds.ByName(typ); found && len(k.SQL) > 0 {
				sqlType, ok = k.CanonicalSQL(), true
			}
			en := str(val, "枚举")
			if en != "" && typ == "" {
				sqlType, ok = "int4", true
			}
			if !ok {
				d.Err(key.Line, "表 %s 的字段 %s: 类型 %q 无法识别(%s)", name, key.Value, typ, kinds.Names())
			}
			col := str(val, "列")
			if col != "" && str(val, "键") != "" {
				d.Err(key.Line, "表 %s 的字段 %s: 键(JSON 里的名字)和 列(物理表的列)只能写一个", name, key.Value)
			}
			st.Fields = append(st.Fields, spec.StorageField{Name: key.Value, Key: str(val, "键"), Type: sqlType, Nullable: nullable, Column: col})
			d.Column(key.Value, str(val, "说明"), en)
			if enc := str(val, "编码"); enc != "" { // 整数里的日期(YYYYMMDD): 和普通的表的列一样
				d.Encoding(key.Line, key.Value, enc)
			}
		}
	}
	if len(st.Fields) == 0 {
		d.Err(n.Line, "表 %s: 存储: JSON版本 需要 字段", name)
	}
	for _, k := range d.PrimaryKey() {
		found := false
		for _, f := range st.Fields {
			found = found || f.Name == k
		}
		if !found {
			d.Err(n.Line, "表 %s: 主键 %s 不在 字段 里", name, k)
		}
	}
	return st
}

// jsonVersionsSteps: 单元库的来源单元 JSON版本 的部件(展开 / 每键最新 / 有效 / 逻辑列)。
func jsonVersionsSteps(st *spec.Storage, nk []string, scan core.Step, lib *cells.Library) ([]core.Step, error) {
	data := core.Col(st.DataColumn)
	var parts cells.SourceParts
	parts.Scan, parts.Keys = scan, nk
	for _, k := range nk { // 键非空: 只对没有声明 可以为空 的键列
		nullable := false
		for _, f := range st.Fields {
			if f.Name == k && f.Nullable {
				nullable = true
			}
		}
		if !nullable {
			parts.NotNull = append(parts.NotNull, k)
		}
	}
	for _, f := range st.Fields {
		k, ok := kinds.BySQL(f.Type)
		if !ok || k.Core == "" {
			return nil, fmt.Errorf("存储字段 %s: 类型 %s 核心代数还不支持", f.Name, f.Type)
		}
		if f.Column != "" && (contains(nk, f.Name) || f.Column == st.DataColumn || f.Column == st.ActionColumn) {
			return nil, fmt.Errorf("存储字段 %s: 外壳列不能是自然键,也不能是 JSON 列/操作列", f.Name)
		}
		if f.Column != "" { // 外壳列: 物理表的列,类型要和声明的一致
			t, ok := core.Schema(scan.Columns).Col(f.Column)
			if !ok {
				return nil, fmt.Errorf("存储字段 %s: 物理表 %s 没有列 %s", f.Name, st.Table, f.Column)
			}
			if t.Kind != k.Core {
				return nil, fmt.Errorf("存储字段 %s: 物理表的列 %s 是 %s,声明的类型是 %s", f.Name, f.Column, t.Kind, k.Core)
			}
			parts.Expand = append(parts.Expand, core.NamedExpr{Name: f.Name, Expr: core.Col(f.Column)})
		} else {
			parts.Expand = append(parts.Expand, core.NamedExpr{Name: f.Name, Expr: core.JSONField(data, f.JSONKey(), core.Type{Kind: k.Core})})
		}
		parts.Logical = append(parts.Logical, core.NamedExpr{Name: f.Name, Expr: core.Col(f.Name)})
	}
	parts.Expand = append(parts.Expand, core.NamedExpr{Name: "_action", Expr: core.Col(st.ActionColumn)})
	for _, c := range st.OrderBy {
		parts.Expand = append(parts.Expand, core.NamedExpr{Name: "_o_" + c, Expr: core.Col(c)})
		parts.Order = append(parts.Order, core.SortKey{Col: "_o_" + c, Desc: true})
	}
	parts.Names = map[string]*core.Expr{
		"操作":    core.Col("_action"),
		"删除值":   core.Const(st.DeleteValue, core.Type{Kind: core.Int32}),
		"逻辑删除":  core.Const(nil, core.Type{Kind: core.Text}),
		"逻辑删除值": core.Const("", core.Type{Kind: core.Text}),
	}
	if ld := st.LogicalDelete; ld != nil {
		parts.Expand = append(parts.Expand, core.NamedExpr{Name: "_logical_delete", Expr: core.JSONField(data, ld.Key, core.Type{Kind: core.Text})})
		parts.Names["逻辑删除"] = core.Col("_logical_delete")
		parts.Names["逻辑删除值"] = core.Const(ld.Value, core.Type{Kind: core.Text})
	}
	steps, err := lib.Source("JSON版本", parts)
	if err != nil || st.State == "" {
		return steps, err
	}
	for i := range steps { // 每键最新 从当前状态表读(core/state.go;参考求值器照常取最新)
		if steps[i].Kind == "pick_per_key" {
			steps[i].State = st.State
		}
	}
	return steps, nil
}

// StateTable: 逻辑表的当前状态表的名字(factory_ 是工厂保留的前缀)。
func StateTable(view string) string { return "factory_current_" + view }

// jsonVersionsRow: 逻辑表的一行 → 物理表的一行(数据列的 JSON + 操作列)。登记顺序列由验收按行的顺序合成。
func jsonVersionsRow(st *ir.Storage, row map[string]any, enumDB func(col string, v any) (any, error)) (Physical, error) {
	keyOf, colOf := map[string]string{}, map[string]string{}
	for _, f := range st.Fields {
		keyOf[f.Name] = f.Key
		if f.Column != "" {
			colOf[f.Name] = f.Column
		}
	}
	data := map[string]any{}
	phys := map[string]any{}
	action := int64(0)
	for col, v := range row {
		if col == spec.FixtureAction {
			switch v {
			case spec.ActionDelete:
				action = st.DeleteValue
			case spec.ActionLogicalDelete:
				data[st.LogicalDeleteKey] = st.LogicalDeleteValue
			default:
				return Physical{}, fmt.Errorf("%s: unknown action %v", spec.FixtureAction, v)
			}
			continue
		}
		key, ok := keyOf[col]
		if !ok {
			return Physical{}, fmt.Errorf("%s has no field %s", st.View, col)
		}
		x, err := enumDB(col, v)
		if err != nil {
			return Physical{}, err
		}
		if c, ok := colOf[col]; ok {
			phys[c] = x
			continue
		}
		data[key] = x
	}
	b, err := json.Marshal(data)
	if err != nil {
		return Physical{}, err
	}
	phys[st.DataColumn], phys[st.ActionColumn] = string(b), action
	return Physical{Values: phys, Order: true}, nil
}

func get(n *yaml.Node, key string) *yaml.Node {
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

func str(n *yaml.Node, key string) string {
	if v := get(n, key); v != nil {
		return strings.TrimSpace(v.Value)
	}
	return ""
}

func list(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	var out []string
	for _, x := range n.Content {
		out = append(out, strings.TrimSpace(x.Value))
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
