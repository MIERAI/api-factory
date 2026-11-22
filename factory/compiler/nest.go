package compiler

// 一对多: 嵌套(datasource.nests → 核心的 nest)和子聚合(datasource.child_aggs → 核心的 child_agg)。
//
//   - 子表必须在同一个库里(db/schema 里的普通的表,或 存储: JSON版本 等来源单元展开的逻辑表: 子行 = 每个自然键最新的一行)
//   - 分库: 父表和子表都分库时,条件里要有「父表的分库键 = 子表的分库键」(子行在同一个库里);只有一边分库 → 编译失败
//   - 嵌套的顺序: 排序的列 + 条件里子表的列要覆盖子表的主键 / 唯一键(核心的 DRC: 子行的顺序是全序)
//   - 加的列: 嵌套 = 子行的列表(目录里的类型是 NestType: 只能输出,不能筛选 / 排序);子聚合 = 标量(之后和来源的列一样)

import (
	"fmt"
	"strings"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/spec"
	"apifactory/factory/where"
)

// NestType: 目录里嵌套加的列的类型(不是 SQL 的类型: 设计编译器生成 proto 的子行消息,筛选 / 排序不能用)。
const NestType = "nest"

// childSteps: 嵌套 / 子聚合的步骤,cols(来源 + 关联加的列)上加列。shardCols: 父行里来自分库键的列。
func childSteps(s *spec.Spec, cat *catalog, keys map[string][][]string, cols []catColumn, shardCols map[string]bool, lib *cells.Library) ([]catColumn, []core.Step, error) {
	table := s.Datasource.Table
	entity := func(t string) spec.Entity {
		if s.Domain == nil {
			return spec.Entity{}
		}
		return s.Domain.Entities[t]
	}
	has := func(n string) bool {
		for _, c := range cols {
			if c.Name == n {
				return true
			}
		}
		return false
	}
	// common: 子表、条件、子表上的固定条件、分库
	// lookups: 嵌套里的关联(子行上的多对一查找): 子表的来源 = scan(子表) + lookup …,加的列之后和子表的列一样
	common := func(at, child string, on, wh []string, lookups []spec.Lookup) (core.Step, []catColumn, error) {
		st := core.Step{Table: child, From: at, Unique: keys[child]}
		other, ok := cat.tables[child]
		if !ok {
			return st, nil, fmt.Errorf("%s: 表 %q 不在 db/schema 里", at, child)
		}
		if err := relatedSource(s, &st, cat, lib); err != nil { // 逻辑表: 来源的展开 + 唯一键 = 自然键
			return st, nil, fmt.Errorf("%s: %w", at, err)
		}
		for _, c := range other {
			t, err := coreType(child, c)
			if err != nil {
				return st, nil, fmt.Errorf("%s: %w", at, err)
			}
			st.Columns = append(st.Columns, core.NamedType{Name: c.Name, Type: t})
		}
		shardOK := false
		for _, x := range on {
			a, b, ok := strings.Cut(x, "=")
			a, b = strings.TrimSpace(a), strings.TrimSpace(b)
			if !ok || a == "" || b == "" || strings.Contains(b, "=") {
				return st, nil, fmt.Errorf("%s: 条件 %q 写成 \"当前行的列 = %s 的列\"", at, x, child)
			}
			if !has(a) {
				return st, nil, fmt.Errorf("%s: 条件 %q: %s 不是 %s 的列(也不是关联加的列)", at, x, a, table)
			}
			if _, ok := cat.column(child, b); !ok {
				return st, nil, fmt.Errorf("%s: 条件 %q: %s 不是 %s 的列", at, x, b, child)
			}
			if sb := entity(child).ShardBy; sb != "" && b == sb && shardCols[a] {
				shardOK = true
			}
			st.On = append(st.On, core.JoinKey{Col: a, Key: b})
		}
		switch src, dst := entity(table).ShardBy, entity(child).ShardBy; {
		case src == "" && dst != "":
			return st, nil, fmt.Errorf("%s: %s 按 %s 分库,%s 不分库: 子表必须和父表在同一个库里", at, child, dst, table)
		case src != "" && dst == "":
			return st, nil, fmt.Errorf("%s: %s 按 %s 分库,%s 不分库: 分库的表的子表也要按同一个键分库", at, table, src, child)
		case src != "" && !shardOK:
			return st, nil, fmt.Errorf("%s: 分库的表和子表,条件里要有「%s = %s」(两边的分库键相等 → 子行在同一个库里)", at, src, dst)
		}
		if len(lookups) > 0 {
			if len(st.Source) > 0 {
				return st, nil, fmt.Errorf("%s: 子表 %s 是逻辑表(存储: …),嵌套里的关联还不支持逻辑表的子表", at, child)
			}
			scan, err := scanStep(child, cat)
			if err != nil {
				return st, nil, fmt.Errorf("%s: %w", at, err)
			}
			scan.From = at + " 的子表"
			src := []core.Step{scan}
			childShard := map[string]bool{}
			if sb := entity(child).ShardBy; sb != "" {
				childShard[sb] = true
			}
			ext := other
			for _, l := range lookups {
				lst, next, err := lookupStep(s, cat, keys, lib, l, fmt.Sprintf("%s 的关联 %s", at, l.Name), child, ext, childShard)
				if err != nil {
					return st, nil, err
				}
				if len(lst.Source) > 0 {
					return st, nil, fmt.Errorf("%s 的关联 %s: 对方 %s 是逻辑表(存储: …),嵌套里的关联还不支持逻辑表", at, l.Name, l.Table)
				}
				ext = next
				src = append(src, lst)
			}
			st.Source = src
			st.Columns = nil
			for _, c := range ext {
				t, err := coreType(child, c)
				if err != nil {
					return st, nil, fmt.Errorf("%s: %w", at, err)
				}
				st.Columns = append(st.Columns, core.NamedType{Name: c.Name, Type: t})
			}
			other = ext
		}
		var preds []*core.Expr
		for _, w := range wh {
			e, err := where.ParseOpt(w, WhereTypes(other), whereOpt(s, child))
			if err != nil {
				return st, nil, fmt.Errorf("%s: %w", at, err)
			}
			preds = append(preds, e)
		}
		switch len(preds) {
		case 0:
		case 1:
			st.Pred = preds[0]
		default:
			st.Pred = core.And(preds...)
		}
		return st, other, nil
	}
	var steps []core.Step
	for _, n := range s.Datasource.Nests {
		at := fmt.Sprintf("嵌套 %s", n.Name)
		st, other, err := common(at, n.Table, n.On, n.Where, n.Lookups)
		if err != nil {
			return nil, nil, err
		}
		st.Kind, st.As, st.N = "nest", n.Name, n.Max
		if n.Max <= 0 {
			return nil, nil, fmt.Errorf("%s: 上限必须 > 0", at)
		}
		for _, o := range n.OrderBy {
			f := strings.Fields(o)
			if len(f) != 2 || (f[1] != "asc" && f[1] != "desc") {
				return nil, nil, fmt.Errorf("%s: 排序 %q 写成 \"列 asc|desc\"", at, o)
			}
			if !hasCat(other, f[0]) {
				return nil, nil, fmt.Errorf("%s: 排序的列 %s 不是 %s 的列(也不是嵌套里的关联加的列)", at, f[0], n.Table)
			}
			st.Order = append(st.Order, core.SortKey{Col: f[0], Desc: f[1] == "desc"})
		}
		for _, t := range n.Columns {
			if !hasCat(other, t.From) {
				return nil, nil, fmt.Errorf("%s: 列 %s: %s 没有列 %s(嵌套里的关联加的列也没有)", at, t.Name, n.Table, t.From)
			}
			st.Take = append(st.Take, core.TakeCol{Name: t.Name, Col: t.From})
		}
		if has(n.Name) {
			return nil, nil, fmt.Errorf("%s: 列名 %s 和 %s 的列(或关联加的列)重名", at, n.Name, table)
		}
		logical := s.Domain != nil && s.Domain.Entities[n.Table].Storage != nil
		if err := checkChildOrder(at, st, other, logical); err != nil {
			return nil, nil, err
		}
		cols = append(cols, catColumn{Name: n.Name, Type: NestType, NotNull: true})
		steps = append(steps, st)
	}
	for _, a := range s.Datasource.ChildAggs {
		at := fmt.Sprintf("子聚合 %s", a.Name)
		st, other, err := common(at, a.Table, a.On, a.Where, nil)
		if err != nil {
			return nil, nil, err
		}
		st.Kind, st.Unique = "child_agg", nil
		st.Aggs = []core.Agg{{Name: a.Name, Func: a.Func, Col: a.Column}}
		var colT *core.Type
		sqlType := ""
		if a.Column != "" {
			c, ok := cat.column(a.Table, a.Column)
			if !ok {
				return nil, nil, fmt.Errorf("%s: %s 没有列 %s", at, a.Table, a.Column)
			}
			t, err := coreType(a.Table, c)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", at, err)
			}
			colT, sqlType = &t, c.Type
		}
		t, err := core.AggType(a.Func, colT, true)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", at, err)
		}
		if has(a.Name) {
			return nil, nil, fmt.Errorf("%s: 列名 %s 和已有的列重名", at, a.Name)
		}
		switch { // 目录里的 SQL 类型: count / 整数的 sum = int8;十进制的 sum = numeric;min / max = 原列的类型
		case a.Func == "count" || (a.Func == "sum" && t.Kind == core.Int64):
			sqlType = "int8"
		case a.Func == "sum":
			sqlType = "numeric"
		}
		_ = other
		cols = append(cols, catColumn{Name: a.Name, Type: sqlType, NotNull: !t.Nullable})
		steps = append(steps, st)
	}
	return cols, steps, nil
}

func hasCat(cs []catColumn, n string) bool {
	for _, c := range cs {
		if c.Name == n {
			return true
		}
	}
	return false
}

// checkChildOrder: 嵌套的顺序是全序(和核心的 DRC 同一条规则,这里用设计稿的说法报错)。
// logical: 子表是逻辑表(唯一键 = 自然键: 可以为空的键列里空值自己是一组,一个父行里最多一行 → 也算全序)。
func checkChildOrder(at string, st core.Step, other []catColumn, logical bool) error {
	inOrder, inOn := map[string]bool{}, map[string]bool{}
	for _, o := range st.Order {
		inOrder[o.Col] = true
	}
	for _, k := range st.On {
		inOn[k.Key] = true
	}
	notNull := map[string]bool{}
	for _, c := range other {
		notNull[c.Name] = c.NotNull
	}
	for _, u := range st.Unique {
		all := len(u) > 0
		for _, c := range u {
			all = all && (inOn[c] || (inOrder[c] && (notNull[c] || logical)))
		}
		if all {
			return nil
		}
	}
	return fmt.Errorf("%s: 子行的排序 %v 不能确定顺序(同一个父行的子行可能并列)。排序里加上 %s 的主键 / 唯一键的列"+
		"(条件里的列不用加;%s 的唯一键: %v)", at, orderCols(st.Order), st.Table, st.Table, st.Unique)
}
