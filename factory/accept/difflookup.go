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
}

// fkRef: 表里的 cols 引用 ref 的 keys(逐列对应)。
type fkRef struct {
	cols []string
	ref  *otherTable
	keys []string
}

// initOthers: 从流水线的关联步骤推出关联的表、外键和造数据的顺序。
func (e *diffEnv) initOthers() error {
	e.others, e.fks = nil, map[string][]fkRef{}
	byName := map[string]*otherTable{}
	type origin struct{ table, col string }
	// 列 → 物理的出处(来源的表的列;关联加的列 = 对方的表的列)。JSON版本 的逻辑列没有物理的出处
	from := map[string]origin{}
	var src []core.Step
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
			}
			from = next
		case st.Kind == "lookup":
			ot, ok := byName[st.Table]
			if !ok {
				ot = &otherTable{table: st.Table, cols: st.Columns, shard: e.plan.Shard(st.Table)}
				byName[st.Table] = ot
				e.others = append(e.others, ot)
			}
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
				e.fks[owner] = append(e.fks[owner], fk)
			}
			for _, t := range st.Take {
				from[t.Name] = origin{st.Table, t.Col}
			}
		}
		if st.Kind == "scan" || st.Kind == "lookup" || st.From == "datasource.table" || strings.HasPrefix(st.From, "来源 ") {
			src = append(src, st)
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
	for _, ot := range e.others { // 来源的外键没有引用到的(出处不明)也要造
		if !done[ot.table] {
			done[ot.table] = true
			order = append(order, ot)
		}
	}
	e.others = order
	view, err := core.Check(&core.Pipeline{Steps: src, Params: e.api.Pipeline.Params})
	if err == nil {
		e.view = view
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
		targets, err := e.db.targets(ot.shard)
		if err != nil {
			return err
		}
		ident := pgx.Identifier{ot.table}.Sanitize()
		for _, c := range targets {
			if _, err := c.Exec(e.ctx, "TRUNCATE "+ident+" CASCADE"); err != nil {
				return err
			}
		}
	}
	for _, ot := range e.others {
		e.table = ot.table
		unique, err := e.singleUnique(ot.table)
		if err != nil {
			return err
		}
		var names, holders []string
		for i, c := range ot.cols {
			names = append(names, pgx.Identifier{c.Name}.Sanitize())
			holders = append(holders, fmt.Sprintf("$%d::%s", i+1, c.Type.Kind.SQLType()))
		}
		ident := pgx.Identifier{ot.table}.Sanitize()
		insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(names, ", "), strings.Join(holders, ", "))
		n := diffRows / 2 * max(e.scale, 1)
		for i := 0; i < n; i++ {
			physical := map[string]any{}
			for _, c := range ot.cols {
				physical[c.Name] = e.otherValue(c, i, unique[c.Name])
			}
			e.applyFKs(ot.table, ot.cols, physical, unique, i)
			var args []any
			for _, c := range ot.cols {
				args = append(args, sqlValue(physical[c.Name]))
			}
			conn := e.conn
			if ot.shard != nil {
				if conn, err = e.db.forKey(physical[ot.shard.Column]); err != nil {
					return err
				}
			}
			_, _ = conn.Exec(e.ctx, insert, args...) // 违反约束(唯一键重复等)的行被拒绝,跳过
		}
		ot.rows, ot.rowDB = nil, nil
		targets, err := e.db.targets(ot.shard)
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
			e.copyKey(physical, fk, refRows[e.rnd.Intn(len(refRows))])
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
	for _, ot := range e.others {
		if conn == nil || ot.shard == nil {
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
		out = append(out, sensTarget{desc: name + " 找到的行(总是找不到的实现)", mutate: with(i, func(s *core.Step) { s.Table = "\x00none" })})
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
			if !ok || !ct.Nullable {
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
