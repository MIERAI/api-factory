package accept

// 差分测试的多表: 关联(lookup)的表的数据和覆盖条件。
//
// 造数据按依赖的顺序: 先造被引用的表(链的末端),再造引用它的表,最后是来源的表。
// 外键(关联条件推出来的「这几列 → 对方的键」)的值: 约 80% 从对方实际插入的行里取(整组一起取,关联真的发生),
// 约 20% 取不存在的值(多列的键: 一半是"每一列单独存在、组合不存在"),可以为空的外键约 1/5 为 NULL。
// 对方的表: 单列的唯一键给唯一的值;多列的唯一键每一列都取小值域(组合重复的行被数据库拒绝)——
// 这样同一个区画番号出现在好几个仓库里,"少比较一列"的错误实现才看得出来。
//
// 覆盖条件(sensTargets 的形式: 参考求值器的"错误实现",要 ≥3 个请求能区分):
//   - 找不到时 保留 ↔ 丢弃(找不到的行出现在结果里 / 丢弃了 ≥1 行)
//   - 总是找不到(找到的行出现在结果里)
//   - 多列的键少比较一列(每一列)
//   - 当前行的关联列可以为空、保留: 关联列为空的行出现在结果里;对方的键列也可以为空: NULL 当作相等

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
)

// otherTable: 关联的一张表(同一张表被关联好几次也只有一份数据)。
type otherTable struct {
	table string
	cols  []core.NamedType
	rows  []core.Row
	rowDB []*pgx.Conn
	shard *ir.Shard
	lt    *logicalTable // 对方是逻辑表(来源单元展开的表): 版本写进物理表(diffsource.go)
	// withParent: 分库: 随父(没有分库键): 每一行写进一个随机的分库(约 1/3 每个分库都写一份)——
	// 父行在别的库里时找不到(查错了库的实现看得出来)
	withParent bool
	// pinned: 关联的条件里 "对方的列 = 常量" 的列 → 常量(约 2/3 的行取这个值,否则几乎没有行能匹配)
	pinned map[string][]any
	// asofCols: 按日期的关联的开始 / 结束列(参照是当前行的列): 约一半的行取和来源的唯一的日期一样分布的值
	// (来源的日期列是唯一列时取序号: 中间 ± n/2 天;对方只取小值域就几乎碰不到参照)
	asofCols map[string]bool
}

// fkRef: 表里的 cols 引用 ref 的 keys(逐列对应)。
type fkRef struct {
	cols []string
	ref  *otherTable
	keys []string
	// 按日期的关联(参照 = 主人的列 refCol): 取存在的键时,参照的值多半落在那一行的开始 / 结束的前后
	refCol, start, end string
}

// origin: 列的物理的出处(表的列)。
type origin struct{ table, col string }

// initOthers: 从流水线的关联步骤推出关联的表、外键和造数据的顺序(嵌套 / 子聚合的子表: initChildren)。
func (e *diffEnv) initOthers() error {
	e.others, e.fks = nil, map[string][]fkRef{}
	byName := map[string]*otherTable{}
	// 列 → 物理的出处(来源的表的列;关联加的列 = 对方的表的列)。JSON版本 的逻辑列没有物理的出处
	from := map[string]origin{}
	var src []core.Step
	var childOwners []string // 嵌套里的关联的外键的主人(子表)
	for _, st := range e.api.Pipeline.Steps {
		switch {
		case st.Kind == "scan":
			for _, c := range st.Columns {
				from[c.Name] = origin{st.Table, c.Name}
			}
		case st.Kind == "project" && (st.From == "datasource.table" || strings.HasPrefix(st.From, "来源 ")):
			next := map[string]origin{}
			for _, x := range st.Project {
				if x.Expr.Op == "col" {
					if o, ok := from[x.Expr.Name]; ok {
						next[x.Name] = o
					}
				}
				if x.Expr.Op == "json_field" && e.plan.Storage(e.api.Table) != nil { // 逻辑表的列: 出处 = 逻辑表(外键写进 JSON: applyMainFKs)
					next[x.Name] = origin{e.api.Table, x.Name}
				}
			}
			from = next
		case st.Kind == "lookup":
			ot, ok := byName[st.Table]
			if !ok {
				ot = &otherTable{table: st.Table, cols: st.Columns, shard: e.plan.Shard(st.Table), lt: e.newLogical(st), withParent: e.plan.WithParent(st.Table)}
				byName[st.Table] = ot
				e.others = append(e.others, ot)
			}
			ot.pin(st.Pred)
			ot.asOf(st)
			var fk fkRef
			owner := ""
			for _, k := range st.On {
				o, ok := from[k.Col]
				if !ok || (owner != "" && o.table != owner) { // 出处不明 / 来自两张表: 不造外键(随机的值)
					owner = "\x00"
					break
				}
				owner = o.table
				fk.cols = append(fk.cols, o.col)
				fk.keys = append(fk.keys, k.Key)
			}
			if owner != "\x00" && owner != "" {
				fk.ref = ot
				if a := st.AsOf; a != nil && a.Ref != nil && a.Ref.Op == "col" {
					if o, ok := from[a.Ref.Name]; ok && o.table == owner {
						fk.refCol, fk.start, fk.end = o.col, a.Start, a.End
					}
				}
				e.fks[owner] = append(e.fks[owner], fk)
			}
			for _, t := range st.Take {
				from[t.Name] = origin{st.Table, t.Col}
			}
		}
		if st.Kind == "scan" || st.Kind == "lookup" || st.From == "datasource.table" || strings.HasPrefix(st.From, "来源 ") {
			src = append(src, st)
		}
		if (st.Kind == "nest" || st.Kind == "child_agg") && e.plan.Storage(st.Table) == nil {
			// 嵌套里的关联: 子表的来源 = scan(子表) + lookup …;外键的主人 = 子表(子行造好之后由 loadChildren 套用)
			sub := map[string]origin{}
			for _, s := range st.Source {
				switch s.Kind {
				case "scan":
					for _, c := range s.Columns {
						sub[c.Name] = origin{s.Table, c.Name}
					}
				case "lookup":
					ot, ok := byName[s.Table]
					if !ok {
						ot = &otherTable{table: s.Table, cols: s.Columns, shard: e.plan.Shard(s.Table), withParent: e.plan.WithParent(s.Table)}
						byName[s.Table] = ot
						e.others = append(e.others, ot)
					}
					ot.pin(s.Pred)
					ot.asOf(s)
					var fk fkRef
					owner := ""
					for _, k := range s.On {
						o, ok := sub[k.Col]
						if !ok || (owner != "" && o.table != owner) {
							owner = "\x00"
							break
						}
						owner = o.table
						fk.cols = append(fk.cols, o.col)
						fk.keys = append(fk.keys, k.Key)
					}
					if owner != "\x00" && owner != "" {
						fk.ref = ot
						if a := s.AsOf; a != nil && a.Ref != nil && a.Ref.Op == "col" {
							if o, ok := sub[a.Ref.Name]; ok && o.table == owner {
								fk.refCol, fk.start, fk.end = o.col, a.Start, a.End
							}
						}
						e.fks[owner] = append(e.fks[owner], fk)
						childOwners = append(childOwners, owner)
					}
					for _, t := range s.Take {
						sub[t.Name] = origin{s.Table, t.Col}
					}
				}
			}
		}
	}
	// 造数据的顺序: 被引用的表在前(深度优先,引用的表的外键先处理)
	var order []*otherTable
	done := map[string]bool{}
	var visit func(t string, depth int) error
	visit = func(t string, depth int) error {
		if depth > len(e.others)+1 {
			return fmt.Errorf("关联的表互相引用(%s)", t)
		}
		for _, fk := range e.fks[t] {
			if !done[fk.ref.table] {
				if err := visit(fk.ref.table, depth+1); err != nil {
					return err
				}
			}
		}
		if ot, ok := byName[t]; ok && !done[t] {
			done[t] = true
			order = append(order, ot)
		}
		return nil
	}
	if err := visit(e.table, 0); err != nil {
		return err
	}
	for _, t := range childOwners { // 子表引用的表(嵌套里的关联)也要在子表之前造(loadOthers 在 loadChildren 之前)
		if err := visit(t, 0); err != nil {
			return err
		}
	}
	if e.api.Table != e.table { // 数据来源是逻辑表: 外键的主人是逻辑表名
		if err := visit(e.api.Table, 0); err != nil {
			return err
		}
	}
	for _, ot := range e.others { // 来源的外键没有引用到的(出处不明)也要造
		if !done[ot.table] {
			done[ot.table] = true
			order = append(order, ot)
		}
	}
	e.others = order
	e.initChildren(from)
	view, err := core.Check(&core.Pipeline{Steps: src, Params: e.api.Pipeline.Params})
	if err == nil {
		e.view = view
	}
	var main []core.Step // 数据来源部分(逻辑表的列: 外键写进 JSON 时的类型)
	for _, st := range src {
		if st.Kind != "lookup" {
			main = append(main, st)
		}
	}
	if s, err := core.Check(&core.Pipeline{Steps: main, Params: e.api.Pipeline.Params}); err == nil {
		e.mainLogical = s
	}
	return nil
}

// loadOthers: 清空关联的表,按顺序造数据(插入 → 读回实际的行)。
func (e *diffEnv) loadOthers() error {
	if len(e.others) == 0 {
		return nil
	}
	saved := e.table // value / scalarFor 按 e.table 查枚举
	defer func() { e.table = saved }()
	for _, ot := range e.others {
		targets, err := e.tableTargets(ot.shard, ot.withParent)
		if err != nil {
			return err
		}
		ident := pgx.Identifier{ot.table}.Sanitize()
		for _, c := range targets {
			if ot.lt != nil { // 逻辑表: 物理表由 loadStorageRows 清空
				break
			}
			if _, err := c.Exec(e.ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
				return err
			}
		}
	}
	for _, ot := range e.others {
		e.table = ot.table
		if ot.lt != nil {
			if err := e.loadOtherLogical(ot); err != nil {
				return err
			}
			continue
		}
		unique, err := e.singleUnique(ot.table)
		if err != nil {
			return err
		}
		dom, err := e.checkDomains(ot.table, ot.cols) // 关联的表的 CHECK(diffnest.go)
		if err != nil {
			return err
		}
		gen, err := e.generatedCols(ot.table)
		if err != nil {
			return err
		}
		var names, holders []string
		var ins []core.NamedType
		for _, c := range ot.cols {
			if gen[c.Name] {
				continue
			}
			ins = append(ins, c)
			names = append(names, pgx.Identifier{c.Name}.Sanitize())
			holders = append(holders, fmt.Sprintf("$%d::%s", len(holders)+1, c.Type.Kind.SQLType()))
		}
		ident := pgx.Identifier{ot.table}.Sanitize()
		insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(names, ", "), strings.Join(holders, ", "))
		n := diffRows / 2 * max(e.scale, 1)
		for i := 0; i < n; i++ {
			physical := map[string]any{}
			for _, c := range ins {
				if vs := ot.pinned[c.Name]; len(vs) > 0 && !unique[c.Name] && e.rnd.Intn(3) != 0 { // 对方的列 = 常量
					physical[c.Name] = vs[e.rnd.Intn(len(vs))]
					continue
				}
				if ot.asofCols[c.Name] && !unique[c.Name] && e.rnd.Intn(2) == 0 { // 按日期的关联(参照 = 列)
					if v := e.scalar(c.Type.Kind, true, e.rnd.Intn(diffRows*max(e.scale, 1))); v != nil {
						physical[c.Name] = v
						continue
					}
				}
				if e.boundaryCol(ot.table, c.Name) && !unique[c.Name] && e.rnd.Intn(3) != 0 { // 按日期的关联(参照 = 今天): 探针的边界
					if v := e.boundaryOf(c.Name); v != nil {
						physical[c.Name] = v
						continue
					}
				}
				if d := dom[c.Name]; d != nil {
					physical[c.Name] = e.relatedValue(c, i, unique[c.Name], d)
				} else {
					physical[c.Name] = e.otherValue(c, i, unique[c.Name])
				}
			}
			e.applyFKs(ot.table, ot.cols, physical, unique, i)
			var args []any
			for _, c := range ins {
				args = append(args, sqlValue(physical[c.Name]))
			}
			conn := e.conn
			if ot.shard != nil {
				if conn, err = e.db.forKey(physical[ot.shard.Column]); err != nil {
					return err
				}
			}
			if ot.withParent { // 分库: 随父: 随机的一个分库,约 1/3 每个分库都写
				shards, err := e.db.shards()
				if err != nil {
					return err
				}
				if e.rnd.Intn(3) == 0 {
					for _, c := range shards {
						_, _ = c.Exec(e.ctx, insert, args...)
					}
					continue
				}
				conn = shards[e.rnd.Intn(len(shards))]
			}
			_, _ = conn.Exec(e.ctx, insert, args...) // 违反约束(唯一键重复等)的行被拒绝,跳过
		}
		ot.rows, ot.rowDB = nil, nil
		targets, err := e.tableTargets(ot.shard, ot.withParent)
		if err != nil {
			return err
		}
		var sel []string
		for _, c := range ot.cols {
			sel = append(sel, pgx.Identifier{c.Name}.Sanitize()+"::text")
		}
		for _, conn := range targets {
			rows, err := conn.Query(e.ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(sel, ", "), ident))
			if err != nil {
				return err
			}
			for rows.Next() {
				vals := make([]*string, len(ot.cols))
				ptrs := make([]any, len(vals))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					rows.Close()
					return err
				}
				r := core.Row{}
				for i, c := range ot.cols {
					v, err := core.FromPGText(vals[i], c.Type.Kind)
					if err != nil {
						rows.Close()
						return err
					}
					r[c.Name] = v
				}
				ot.rows = append(ot.rows, r)
				ot.rowDB = append(ot.rowDB, conn)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
	}
	return nil
}

// otherValue: 关联的表的一列。唯一列 = 唯一的值;其他 = 类型的小值域(不用来源的表的值域 / 常量的规则)。
func (e *diffEnv) otherValue(c core.NamedType, i int, unique bool) any {
	if c.Type.Nullable && e.rnd.Intn(5) == 0 {
		return nil
	}
	if c.Type.Kind == core.JSON {
		return "{}"
	}
	return e.scalar(c.Type.Kind, unique, i)
}

// singleUnique: 单列的主键 / 唯一约束的列(给唯一的值)。多列的唯一键不在这里(每列取小值域,组合重复的行被拒绝)。
func (e *diffEnv) singleUnique(table string) (map[string]bool, error) {
	rows, err := e.conn.Query(e.ctx, `
		SELECT array_agg(kcu.column_name::text ORDER BY kcu.ordinal_position)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
		WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
		GROUP BY tc.constraint_name`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var set []string
		if err := rows.Scan(&set); err != nil {
			return nil, err
		}
		if len(set) == 1 {
			out[set[0]] = true
		}
	}
	return out, rows.Err()
}

// applyFKs: 这一行的外键的值(从对方的行里取 / 不存在的值 / NULL)。唯一列(来源的主键等)不改。
func (e *diffEnv) applyFKs(table string, cols []core.NamedType, physical map[string]any, unique map[string]bool, i int) {
	types := core.Schema(cols)
	for _, fk := range e.fks[table] {
		skip := false
		for _, c := range fk.cols {
			skip = skip || unique[c]
		}
		if skip {
			continue
		}
		var nullable []string
		for _, c := range fk.cols {
			if t, ok := types.Col(c); ok && t.Nullable {
				nullable = append(nullable, c)
			}
		}
		refRows := fk.ref.rows
		switch r := e.rnd.Intn(10); {
		case len(nullable) > 0 && r < 2: // NULL(可以为空的列;多列时其他列照常取存在的值)
			if len(refRows) > 0 {
				e.copyKey(physical, fk, refRows[e.rnd.Intn(len(refRows))])
			}
			physical[nullable[e.rnd.Intn(len(nullable))]] = nil
		case r < 8 && len(refRows) > 0: // 存在的键(整组)
			ref := refRows[e.rnd.Intn(len(refRows))]
			e.copyKey(physical, fk, ref)
			if fk.refCol != "" && !unique[fk.refCol] && e.rnd.Intn(4) != 0 { // 按日期: 参照落在这一行的开始 / 结束的前后
				if v := nearAsOf(ref, fk, e.rnd.Intn(2), []int{-1, 0, 0, 1}[e.rnd.Intn(4)]); v != nil {
					physical[fk.refCol] = v
				}
			}
		default: // 不存在的键
			if len(fk.cols) > 1 && len(refRows) > 1 && e.rnd.Intn(2) == 0 { // 每一列单独存在,组合不存在(多半)
				e.copyKey(physical, fk, refRows[e.rnd.Intn(len(refRows))])
				j := e.rnd.Intn(len(fk.cols))
				physical[fk.cols[j]] = refRows[e.rnd.Intn(len(refRows))][fk.keys[j]]
				continue
			}
			j := e.rnd.Intn(len(fk.cols))
			if len(refRows) > 0 {
				e.copyKey(physical, fk, refRows[e.rnd.Intn(len(refRows))])
			}
			t, _ := types.Col(fk.cols[j])
			physical[fk.cols[j]] = missingKey(t.Kind, i)
		}
	}
}

func (e *diffEnv) copyKey(physical map[string]any, fk fkRef, ref core.Row) {
	for j, c := range fk.cols {
		physical[c] = ref[fk.keys[j]]
	}
}

// missingKey: 一定不在对方的键里的值(对方的唯一列是序号 / 带后缀的文本 / 值域中间 ± 天)。
func missingKey(k core.Kind, i int) any {
	switch k {
	case core.Int32, core.Int64:
		return int64(90000 + i)
	case core.Text:
		return fmt.Sprintf("∉%d", i%7)
	case core.Decimal:
		return big.NewRat(int64(90000+i), 1)
	case core.Date, core.Timestamp:
		return time.Date(2099, 1, 1+i%28, 0, 0, 0, 0, time.UTC)
	}
	return missing(k)
}

// evalTables: 参考求值器的输入(请求能看到的行): 来源的表 + 关联的表(分库时只用请求的库里的行)。
func (e *diffEnv) evalTables(ps core.Params) map[string][]core.Row {
	var conn *pgx.Conn
	if sh := e.api.Shard; sh != nil {
		conn, _ = e.db.forKey(ps[sh.Field])
	}
	return e.tablesIn(conn, e.rowsFor(ps))
}

// tablesIn: 来源的行 main + 关联的表在库 conn 里的行(conn = nil: 全部)。
func (e *diffEnv) tablesIn(conn *pgx.Conn, main []core.Row) map[string][]core.Row {
	m := map[string][]core.Row{e.table: main}
	e.childTablesIn(conn, m)
	for _, ot := range e.others {
		if ot.lt != nil { // 逻辑表: 参考求值器的输入是物理表的行(来源的子流水线自己算)
			m[ot.lt.st.Table] = ot.lt.phys
			continue
		}
		if conn == nil || (ot.shard == nil && !ot.withParent) {
			m[ot.table] = ot.rows
			continue
		}
		var rs []core.Row
		for i, r := range ot.rows {
			if ot.rowDB[i] == conn {
				rs = append(rs, r)
			}
		}
		m[ot.table] = rs
	}
	return m
}

// lookupTargets: 关联的覆盖条件(错误实现 → ≥3 个请求能区分)。
func (e *diffEnv) lookupTargets() []sensTarget {
	p := e.api.Pipeline
	var out []sensTarget
	with := func(i int, f func(st *core.Step)) func(p *core.Pipeline) *core.Pipeline {
		return func(p *core.Pipeline) *core.Pipeline {
			mut := *p
			mut.Steps = append([]core.Step(nil), p.Steps...)
			st := mut.Steps[i]
			st.On = append([]core.JoinKey(nil), st.On...)
			f(&st)
			mut.Steps[i] = st
			return &mut
		}
	}
	for i, st := range p.Steps {
		if st.Kind != "lookup" {
			continue
		}
		i, st := i, st
		name := st.From
		keep := st.Missing != "drop"
		flip := map[bool]string{true: "保留 → 丢弃", false: "丢弃 → 保留"}[keep]
		out = append(out, sensTarget{desc: fmt.Sprintf("%s 的找不到时(%s)", name, flip), mutate: with(i, func(s *core.Step) {
			if keep {
				s.Missing = "drop"
			} else {
				s.Missing = "keep"
			}
		})})
		out = append(out, sensTarget{desc: name + " 找到的行(总是找不到的实现)", mutate: with(i, func(s *core.Step) { s.Table, s.Source = "\x00none", nil })})
		out = append(out, sourceTargets(i, st, name)...)
		for _, m := range asOfMutations(st) {
			m := m
			out = append(out, sensTarget{desc: name + " " + m.desc, mutate: with(i, m.f)})
		}
		if len(st.On) > 1 {
			for j, k := range st.On {
				j := j
				out = append(out, sensTarget{desc: fmt.Sprintf("%s 的键少比较 %s", name, k.Col), mutate: with(i, func(s *core.Step) {
					s.On = append(append([]core.JoinKey(nil), s.On[:j]...), s.On[j+1:]...)
				})})
			}
		}
		before, err := core.Check(&core.Pipeline{Steps: p.Steps[:i], Params: p.Params})
		if err != nil {
			continue
		}
		for _, k := range st.On {
			ct, ok := before.Col(k.Col)
			if !ok || !ct.Nullable || e.fixedCols()[k.Col] || e.narrowedOut(k.Col) { // 请求固定了这一列(必填的等于)/ 之后的条件拒绝空值: 空值的行进不了结果
				continue
			}
			kt, _ := core.Schema(st.Columns).Col(k.Key)
			if keep {
				col := k.Col
				out = append(out, sensTarget{desc: fmt.Sprintf("%s 的关联列 %s 为空的行(保留)", name, col), mutate: func(p *core.Pipeline) *core.Pipeline {
					mut := *p
					mut.Steps = append(append(append([]core.Step(nil), p.Steps[:i]...), core.Filter(core.Not(core.IsNull(core.Col(col))))), p.Steps[i:]...)
					return &mut
				}})
			}
			if kt.Nullable {
				out = append(out, sensTarget{desc: fmt.Sprintf("%s 的 NULL 当作相等(%s)", name, k.Col), mutate: with(i, func(s *core.Step) { s.NullEq = true })})
			}
		}
	}
	return out
}

// nearAsOf: 按日期的关联的参照的值: 对方的那一行的开始(which = 0)或结束(which = 1,有结束列而且不为空)前后 off 天(时间: 秒)。
func nearAsOf(ref core.Row, fk fkRef, which, off int) any {
	base := ref[fk.start]
	if which == 1 && fk.end != "" && ref[fk.end] != nil {
		base = ref[fk.end]
	}
	switch v := base.(type) {
	case time.Time:
		if v.Hour() == 0 && v.Minute() == 0 && v.Second() == 0 && v.Nanosecond() == 0 {
			return v.AddDate(0, 0, off)
		}
		return v.Add(time.Duration(off) * time.Second)
	case int64:
		return v + int64(off)
	}
	return nil
}

// asOfMutation: lookup 的一个"错误实现"(按日期 / 对方的条件)。
type asOfMutation struct {
	desc string
	f    func(s *core.Step)
}

// asOfMutations: 按日期的关联的错误实现: 开始 ≤ 写成 <、结束的含 / 不含反了、不看结束、取开始最小的、
// 参照 = 今天 时读错时钟 / 在 UTC 取日期;对方的条件去掉。
func asOfMutations(st core.Step) []asOfMutation {
	var out []asOfMutation
	if st.Pred != nil {
		out = append(out, asOfMutation{"的对方的条件(去掉)", func(s *core.Step) { s.Pred = nil }})
	}
	a := st.AsOf
	if a == nil {
		return out
	}
	mut := func(desc string, f func(x *core.AsOf)) {
		out = append(out, asOfMutation{desc, func(s *core.Step) {
			x := *s.AsOf
			f(&x)
			s.AsOf = &x
		}})
	}
	mut("的按日期: 开始 ≤ 参照 写成 <", func(x *core.AsOf) { x.Mutant = "start_lt" })
	mut("的按日期: 取了开始最小的行", func(x *core.AsOf) { x.Mutant = "min" })
	if a.End != "" {
		mut("的按日期: 结束的当天(含 ↔ 不含)", func(x *core.AsOf) { x.Mutant = "end_flip" })
		mut("的按日期: 不看结束", func(x *core.AsOf) { x.Mutant = "no_end" })
	}
	if core.UsesNow(a.Ref) {
		mut("的按日期: 参照读错了时钟(晚 1 天)", func(x *core.AsOf) { x.Ref = shiftNow(x.Ref) })
		if hasZone(a.Ref) {
			mut("的按日期: 参照的时区(换成 UTC)", func(x *core.AsOf) { x.Ref = rezone(x.Ref, "UTC") })
		}
	}
	return out
}

func hasZone(x *core.Expr) bool {
	if x == nil {
		return false
	}
	if x.Zone != "" && x.Zone != "UTC" {
		return true
	}
	for _, a := range x.Args {
		if hasZone(a) {
			return true
		}
	}
	return false
}

// pin: 关联的条件(对方的列上)里顶层的 "列 = 常量" → 造数据时多半取这个常量。
func (ot *otherTable) pin(pred *core.Expr) {
	if pred == nil {
		return
	}
	var walk func(x *core.Expr)
	walk = func(x *core.Expr) {
		switch {
		case x.Op == "and":
			for _, a := range x.Args {
				walk(a)
			}
		case x.Op == "cmp" && x.Name == "=" && len(x.Args) == 2:
			a, b := x.Args[0], x.Args[1]
			if a.Op == "const" {
				a, b = b, a
			}
			if a.Op == "col" && b.Op == "const" && b.Value != nil {
				if ot.pinned == nil {
					ot.pinned = map[string][]any{}
				}
				for _, v := range ot.pinned[a.Name] {
					if core.Canonical(v) == core.Canonical(b.Value) {
						return
					}
				}
				ot.pinned[a.Name] = append(ot.pinned[a.Name], b.Value)
			}
		}
	}
	walk(pred)
}

// asOf: 按日期的关联(参照是当前行的列)的开始 / 结束列。
func (ot *otherTable) asOf(st core.Step) {
	a := st.AsOf
	if a == nil || a.Ref == nil || a.Ref.Op != "col" {
		return
	}
	if ot.asofCols == nil {
		ot.asofCols = map[string]bool{}
	}
	ot.asofCols[a.Start] = true
	if a.End != "" {
		ot.asofCols[a.End] = true
	}
}

// tableTargets: 表要清空 / 读回的库: 分库的表、分库: 随父 的表 = 所有的库;其他 = 主库。
func (e *diffEnv) tableTargets(sh *ir.Shard, withParent bool) ([]*pgx.Conn, error) {
	if withParent {
		return e.db.all()
	}
	return e.db.targets(sh)
}
