package accept

// 差分测试的一对多: 嵌套(nest)和子聚合(child_agg)的子表的数据和覆盖条件;关联的表 / 子表的约束(CHECK)。
//
// 子表在父表(来源的表,或关联的表)之后造: 每个父行的子行数在 0 / 1 / 几个 / 正好上限 / 上限 + 1 之间分布
// (子聚合只用的子表: 上限按 4 算),另外几行关联不到父行(孤立的子行;有外键约束时被数据库拒绝)。
// 子表的唯一键: 包含关联列的复合键 → 其余的列给"父行内的序号"(line_no = 1, 2, …);单列的唯一键 → 全局的序号。
//
// 约束: 关联的表 / 子表的单列 CHECK(例: sku ~ '^[A-Z0-9-]{3,32}$'、qty > 0)从数据库读出来,
// 候选值(类型的值域 + 常见的写法)逐个交给数据库判定,只用通过的值 —— 否则这些表的行几乎全被拒绝。
// 类型的值域全部通过的列不受影响(和以前一样)。生成列(GENERATED ALWAYS)不插入。
//
// 覆盖条件(sensTargets 的形式: 参考求值器的"错误实现",要 ≥3 个请求能区分):
//   - 嵌套: 空列表 → NULL;上限差一(> 写成 ≥ / 没有多取一行);去掉子表的条件;顺序的每一列方向反转、少最后一列;关联列换成另一列
//   - 子聚合: 空集合的 sum → NULL;count = LEFT JOIN 的行数(空集合 = 1);min ↔ max;去掉子表的条件;关联列换成另一列

import (
	"fmt"
	"math/big"
	"math/rand"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/kinds"
)

// childTable: 嵌套 / 子聚合的一张子表(同一张表被用好几次也只有一份数据)。
type childTable struct {
	table  string
	cols   []core.NamedType
	rows   []core.Row
	rowDB  []*pgx.Conn
	shard  *ir.Shard
	max    int      // 子行数的分布用的上限(嵌套的 N;只有子聚合 = 4)
	owner  string   // 父行所在的表(来源的表 / 关联的表)
	fkCols []string // 子表的关联列
	pkCols []string // 父行的列(逐个对应)
}

// initChildren: 从流水线的 nest / child_agg 推出子表(initOthers 之后调用: from = 列 → 物理的出处)。
func (e *diffEnv) initChildren(from map[string]origin) {
	e.children = nil
	byName := map[string]*childTable{}
	for _, st := range e.api.Pipeline.Steps {
		if st.Kind != "nest" && st.Kind != "child_agg" {
			continue
		}
		ct, ok := byName[st.Table]
		if !ok {
			ct = &childTable{table: st.Table, cols: st.Columns, shard: e.plan.Shard(st.Table), max: 4}
			byName[st.Table] = ct
			e.children = append(e.children, ct)
		}
		if st.Kind == "nest" {
			ct.max = st.N
		}
		if ct.owner != "" {
			continue
		}
		owner := ""
		var fk, pk []string
		for _, k := range st.On {
			o, ok := from[k.Col]
			if !ok || (owner != "" && o.table != owner) {
				owner = "\x00"
				break
			}
			owner = o.table
			fk, pk = append(fk, k.Key), append(pk, o.col)
		}
		if owner != "\x00" && owner != "" {
			ct.owner, ct.fkCols, ct.pkCols = owner, fk, pk
		}
	}
}

// parentRows: 父行(来源的表 source = e.rows;关联的表 = 它的行)。
func (e *diffEnv) parentRows(table, source string) []core.Row {
	if table == source {
		return e.rows
	}
	for _, ot := range e.others {
		if ot.table == table {
			return ot.rows
		}
	}
	return nil
}

// childCount: 一个父行的子行数: 0 / 1 / 几个 / 正好上限 / 上限 + 1。
func (e *diffEnv) childCount(max int) int {
	switch r := e.rnd.Intn(20); {
	case r < 4:
		return 0
	case r < 8:
		return 1
	case r < 12:
		return min(2+e.rnd.Intn(2), max)
	case r < 16:
		return max
	default:
		return max + 1
	}
}

// loadChildren: 清空子表,按父行造子行(插入 → 读回实际的行)。来源的表读回之后调用。
func (e *diffEnv) loadChildren() error {
	if len(e.children) == 0 {
		return nil
	}
	saved := e.table
	defer func() { e.table = saved }()
	for _, ct := range e.children {
		targets, err := e.db.targets(ct.shard)
		if err != nil {
			return err
		}
		for _, c := range targets {
			if _, err := c.Exec(e.ctx, "TRUNCATE "+pgx.Identifier{ct.table}.Sanitize()+" CASCADE"); err != nil {
				return err
			}
		}
	}
	for _, ct := range e.children {
		e.table = ct.table
		gen, err := e.generatedCols(ct.table)
		if err != nil {
			return err
		}
		sets, err := e.uniqueSets(ct.table)
		if err != nil {
			return err
		}
		dom, err := e.checkDomains(ct.table, ct.cols)
		if err != nil {
			return err
		}
		fk := map[string]bool{}
		for _, c := range ct.fkCols {
			fk[c] = true
		}
		perParent, global := map[string]bool{}, map[string]bool{} // 父行内的序号 / 全局的序号
		for _, set := range sets {
			var rest []string
			hasFK := false
			for _, c := range set {
				if fk[c] {
					hasFK = true
				} else {
					rest = append(rest, c)
				}
			}
			switch {
			case len(set) == 1 && !hasFK:
				global[set[0]] = true
			case hasFK && len(rest) > 0:
				perParent[rest[len(rest)-1]] = true
			}
		}
		var names, holders []string
		var ins []core.NamedType
		for _, c := range ct.cols {
			if gen[c.Name] {
				continue
			}
			ins = append(ins, c)
			names = append(names, pgx.Identifier{c.Name}.Sanitize())
			holders = append(holders, fmt.Sprintf("$%d::%s", len(holders)+1, c.Type.Kind.SQLType()))
		}
		ident := pgx.Identifier{ct.table}.Sanitize()
		insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", ident, strings.Join(names, ", "), strings.Join(holders, ", "))
		seq := 0
		put := func(parent core.Row, j int) error {
			physical := map[string]any{}
			for _, c := range ins {
				switch {
				case global[c.Name]:
					physical[c.Name] = e.relatedValue(c, seq, true, dom[c.Name])
				case perParent[c.Name]:
					physical[c.Name] = e.relatedValue(c, j, true, dom[c.Name])
				default:
					physical[c.Name] = e.relatedValue(c, seq, false, dom[c.Name])
				}
			}
			seq++
			for i, c := range ct.fkCols {
				if parent == nil { // 孤立的子行: 关联不到任何父行
					t, _ := core.Schema(ct.cols).Col(c)
					physical[c] = missingKey(t.Kind, j)
					continue
				}
				physical[c] = parent[ct.pkCols[i]]
			}
			var args []any
			for _, c := range ins {
				args = append(args, sqlValue(physical[c.Name]))
			}
			conn := e.conn
			if ct.shard != nil {
				var err error
				if conn, err = e.db.forKey(physical[ct.shard.Column]); err != nil {
					return err
				}
			}
			_, _ = conn.Exec(e.ctx, insert, args...) // 违反约束的行被拒绝,跳过
			return nil
		}
		if ct.owner != "" {
			for _, parent := range e.parentRows(ct.owner, saved) {
				skip := false
				for _, c := range ct.pkCols {
					skip = skip || parent[c] == nil
				}
				if skip {
					continue
				}
				n := e.childCount(ct.max)
				for j := 0; j < n; j++ {
					if err := put(parent, j); err != nil {
						return err
					}
				}
			}
			for j := 0; j < 3; j++ {
				if err := put(nil, j); err != nil {
					return err
				}
			}
		} else { // 父行的出处不明(关联列来自计算): 随机的值
			for j := 0; j < diffRows*max(e.scale, 1); j++ {
				if err := put(nil, j); err != nil {
					return err
				}
			}
		}
		if ct.rows, ct.rowDB, err = e.readBack(ct.table, ct.cols, ct.shard); err != nil {
			return err
		}
	}
	return nil
}

// readBack: 一张表的全部行(所有的库;记下每一行在哪个库)。
func (e *diffEnv) readBack(table string, cols []core.NamedType, sh *ir.Shard) ([]core.Row, []*pgx.Conn, error) {
	targets, err := e.db.targets(sh)
	if err != nil {
		return nil, nil, err
	}
	var sel []string
	for _, c := range cols {
		sel = append(sel, pgx.Identifier{c.Name}.Sanitize()+"::text")
	}
	var out []core.Row
	var dbs []*pgx.Conn
	for _, conn := range targets {
		rows, err := conn.Query(e.ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(sel, ", "), pgx.Identifier{table}.Sanitize()))
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			vals := make([]*string, len(cols))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, nil, err
			}
			r := core.Row{}
			for i, c := range cols {
				v, err := core.FromPGText(vals[i], c.Type.Kind)
				if err != nil {
					rows.Close()
					return nil, nil, err
				}
				r[c.Name] = v
			}
			out = append(out, r)
			dbs = append(dbs, conn)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	return out, dbs, nil
}

// generatedCols: 生成列(GENERATED ALWAYS … STORED: 不能插入,值由数据库算)。
func (e *diffEnv) generatedCols(table string) (map[string]bool, error) {
	rows, err := e.conn.Query(e.ctx, `SELECT column_name::text FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = $1 AND is_generated = 'ALWAYS'`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[c] = true
	}
	return out, rows.Err()
}

// uniqueSets: 主键 / 唯一约束的列的组合(按约束里的顺序)。
func (e *diffEnv) uniqueSets(table string) ([][]string, error) {
	rows, err := e.conn.Query(e.ctx, `
		SELECT array_agg(kcu.column_name::text ORDER BY kcu.ordinal_position)
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
		WHERE tc.table_schema = current_schema() AND tc.table_name = $1 AND tc.constraint_type IN ('PRIMARY KEY', 'UNIQUE')
		GROUP BY tc.constraint_name ORDER BY tc.constraint_name`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var set []string
		if err := rows.Scan(&set); err != nil {
			return nil, err
		}
		out = append(out, set)
	}
	return out, rows.Err()
}

// checkedDomain: 一列在 CHECK 约束下的值域。values = 通过的候选值;unique = 第 i 个唯一的值(nil = 用不了序号)。
type checkedDomain struct {
	values []any
	unique func(i int) any
}

// checkCandidates: 候选值(类型的值域之外,常见的写法: 代码、带连字符的编号、正数 …)。
func checkCandidates(k core.Kind) []any {
	var out []any
	if t, ok := kinds.ByCore(k); ok {
		out = append(out, t.Domain...)
	}
	switch k {
	case core.Int32, core.Int64:
		for _, n := range []int64{4, 5, 7, 20, 100, -1} {
			out = append(out, n)
		}
	case core.Decimal:
		for _, s := range []string{"0", "0.01", "2.5", "99.99", "100", "1000"} {
			r, _ := new(big.Rat).SetString(s)
			out = append(out, r)
		}
	case core.Text:
		out = append(out, "ABC", "SKU-1", "SKU-2", "X-9", "AB-123", "Z9Z", "A1", "abc-1", "Tokyo", "T-01", "XY")
	}
	return out
}

// checkDomains: 表的单列 CHECK 约束 → 列的值域(候选值逐个交给数据库判定)。类型的值域全部通过的列不在结果里。
func (e *diffEnv) checkDomains(table string, cols []core.NamedType) (map[string]*checkedDomain, error) {
	rows, err := e.conn.Query(e.ctx, `
		SELECT a.attname::text, pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		WHERE c.contype = 'c' AND c.conrelid = to_regclass($1) AND array_length(c.conkey, 1) = 1
		ORDER BY c.conname`, pgx.Identifier{table}.Sanitize())
	if err != nil {
		return nil, err
	}
	checks := map[string][]string{}
	for rows.Next() {
		var col, def string
		if err := rows.Scan(&col, &def); err != nil {
			rows.Close()
			return nil, err
		}
		if expr, ok := strings.CutPrefix(def, "CHECK "); ok && !strings.Contains(def, "NOT VALID") {
			checks[col] = append(checks[col], expr)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]*checkedDomain{}
	types := core.Schema(cols)
	for col, exprs := range checks {
		t, found := types.Col(col)
		if !found || t.Kind == core.JSON || t.Kind == core.Bool {
			continue
		}
		pass := func(cands []any) ([]any, error) {
			var texts []string
			for _, v := range cands {
				texts = append(texts, fmt.Sprint(sqlValue(v)))
			}
			q := fmt.Sprintf("SELECT u.n FROM unnest($1::text[]) WITH ORDINALITY AS u(v, n), LATERAL (SELECT u.v::%s AS %s) AS t WHERE %s",
				t.Kind.SQLType(), pgx.Identifier{col}.Sanitize(), strings.Join(exprs, " AND "))
			rs, err := e.conn.Query(e.ctx, q, texts)
			if err != nil {
				return nil, err
			}
			defer rs.Close()
			var ok []any
			for rs.Next() {
				var n int64
				if err := rs.Scan(&n); err != nil {
					return nil, err
				}
				ok = append(ok, cands[n-1])
			}
			return ok, rs.Err()
		}
		cands := checkCandidates(t.Kind)
		var patterns []string // 正则的 CHECK: 加上按正则生成的样本
		for _, x := range exprs {
			for _, m := range reCheckRegex.FindAllStringSubmatch(x, -1) {
				patterns = append(patterns, strings.ReplaceAll(m[1], "''", "'"))
			}
		}
		if t.Kind == core.Text {
			for _, pat := range patterns {
				for i := 0; i < 6; i++ {
					if v := regexSample(pat, i); v != "" {
						cands = append(cands, v)
					}
				}
			}
		}
		ok, err := pass(cands)
		if err != nil {
			return nil, fmt.Errorf("%s.%s 的 CHECK: %w", table, col, err)
		}
		if tk, has := kinds.ByCore(t.Kind); has && len(ok) >= len(cands) && len(tk.Domain) > 0 {
			continue // 全部通过: 不受约束
		}
		d := &checkedDomain{values: ok}
		// 唯一的值: 整数 / 十进制 = 序号(从通过的最小正数开始)、文本 = 通过的值 + 序号(试两种写法)
		forms := []func(i int) any{}
		switch t.Kind {
		case core.Int32, core.Int64:
			forms = append(forms, func(i int) any { return int64(i + 1) }, func(i int) any { return int64(i + 1000) })
		case core.Decimal:
			forms = append(forms, func(i int) any { return big.NewRat(int64(i+1), 1) })
		case core.Text:
			for _, pat := range patterns { // 正则: 第 i 个样本(种子 = i)
				pat := pat
				forms = append(forms, func(i int) any { return regexSample(pat, 1000+i) })
			}
			for _, b := range ok {
				b := b.(string)
				forms = append(forms, func(i int) any { return fmt.Sprintf("%s-%02d", b, i) }, func(i int) any { return fmt.Sprintf("%s%02d", b, i) })
			}
		case core.Date, core.Timestamp:
			forms = append(forms, func(i int) any { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i) })
		}
		for _, f := range forms {
			got, err := pass([]any{f(3), f(47), f(512)})
			if err == nil && len(got) == 3 {
				d.unique = f
				break
			}
		}
		out[col] = d
	}
	return out, nil
}

// relatedValue: 关联的表 / 子表的一列的值。有 CHECK 的列从通过的值里取(唯一的列用通过的写法 + 序号)。
func (e *diffEnv) relatedValue(c core.NamedType, i int, unique bool, d *checkedDomain) any {
	if d == nil {
		if unique {
			if c.Type.Kind == core.JSON {
				return "{}"
			}
			return e.scalar(c.Type.Kind, true, i)
		}
		return e.otherValue(c, i, false)
	}
	if unique && d.unique != nil {
		return d.unique(i)
	}
	if c.Type.Nullable && e.rnd.Intn(5) == 0 {
		return nil
	}
	if len(d.values) == 0 {
		return e.scalar(c.Type.Kind, unique, i) // 一个候选值都不通过: 和以前一样(行会被拒绝)
	}
	return d.values[e.rnd.Intn(len(d.values))]
}

// childTablesIn: 子表在库 conn 里的行(conn = nil: 全部)。
func (e *diffEnv) childTablesIn(conn *pgx.Conn, m map[string][]core.Row) {
	for _, ct := range e.children {
		if conn == nil || ct.shard == nil {
			m[ct.table] = ct.rows
			continue
		}
		var rs []core.Row
		for i, r := range ct.rows {
			if ct.rowDB[i] == conn {
				rs = append(rs, r)
			}
		}
		m[ct.table] = rs
	}
}

// presentTyped: 子行的值 → 期待值里的表示(按子行的列的类型: 日期 / 时间分开;子行里没有枚举)。
func presentTyped(t core.Type, v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case time.Time:
		if t.Kind == core.Date {
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	case *big.Rat:
		return core.RatString(x)
	}
	return v
}

// childItems: 子行的列表 → 期待的子消息的列表。
func childItems(c core.Children, t core.Type, msg *ir.Message) []any {
	list := []any{}
	for _, r := range c.Rows {
		item := map[string]any{}
		for _, f := range msg.Fields {
			ft, _ := core.Schema(t.Fields).Col(f.Name)
			v := presentTyped(ft, r[f.Name])
			if v == nil {
				if f.Presence {
					item[f.Name] = "$absent"
				}
				continue
			}
			item[f.Name] = v
		}
		list = append(list, item)
	}
	return list
}

// nestTargets: 嵌套 / 子聚合的覆盖条件(错误实现 → ≥3 个请求能区分)。
func (e *diffEnv) nestTargets() []sensTarget {
	p := e.api.Pipeline
	var out []sensTarget
	with := func(i int, f func(st *core.Step)) func(p *core.Pipeline) *core.Pipeline {
		return func(p *core.Pipeline) *core.Pipeline {
			mut := *p
			mut.Steps = append([]core.Step(nil), p.Steps...)
			st := mut.Steps[i]
			st.On = append([]core.JoinKey(nil), st.On...)
			st.Order = append([]core.SortKey(nil), st.Order...)
			st.Aggs = append([]core.Agg(nil), st.Aggs...)
			f(&st)
			mut.Steps[i] = st
			return &mut
		}
	}
	for i, st := range p.Steps {
		if st.Kind != "nest" && st.Kind != "child_agg" {
			continue
		}
		i, st := i, st
		name := st.From
		if st.Pred != nil {
			out = append(out, sensTarget{desc: name + " 的子表的条件(去掉)", mutate: with(i, func(s *core.Step) { s.Pred = nil })})
		}
		// 关联列换成当前行的另一列(类型相同的第一列)
		// 候选 = 来源的表的列(不用前面的聚合加的列: 和"关联错了列"的 bug 不像)
		if before, err := core.Check(&core.Pipeline{Steps: p.Steps[:i], Params: p.Params}); err == nil && len(st.On) > 0 {
			cur, _ := before.Col(st.On[0].Col)
			src := map[string]bool{}
			for _, c := range e.cols {
				src[c.Name] = true
			}
			ints := func(k core.Kind) bool { return k == core.Int32 || k == core.Int64 }
			for _, c := range before {
				if c.Name != st.On[0].Col && src[c.Name] && (c.Type.Kind == cur.Kind || (ints(c.Type.Kind) && ints(cur.Kind))) {
					col := c.Name
					out = append(out, sensTarget{desc: fmt.Sprintf("%s 的关联列(%s 换成 %s)", name, st.On[0].Col, col), mutate: with(i, func(s *core.Step) { s.On[0].Col = col })})
					break
				}
			}
		}
		if st.Kind == "nest" {
			out = append(out,
				sensTarget{desc: name + " 的空列表(返回 NULL 的实现)", mutate: with(i, func(s *core.Step) { s.Mutant = "empty_null" })},
				sensTarget{desc: name + " 的上限(> 写成 ≥: 正好上限的子行)", mutate: with(i, func(s *core.Step) { s.N-- })},
				sensTarget{desc: name + " 的上限(上限 + 1 的子行)", mutate: with(i, func(s *core.Step) { s.N++ })},
			)
			for j := range st.Order {
				j := j
				out = append(out, sensTarget{desc: fmt.Sprintf("%s 的顺序第 %d 列(%s)的方向", name, j+1, st.Order[j].Col), mutate: with(i, func(s *core.Step) { s.Order[j].Desc = !s.Order[j].Desc })})
			}
			if len(st.Order) > 1 {
				out = append(out, sensTarget{desc: name + " 的顺序少最后一列", mutate: with(i, func(s *core.Step) { s.Order = s.Order[:len(s.Order)-1] })})
			}
			continue
		}
		has := map[string]bool{}
		for _, a := range st.Aggs {
			has[a.Func] = true
		}
		if has["sum"] {
			out = append(out, sensTarget{desc: name + " 的空集合的合计(返回 NULL 的实现)", mutate: with(i, func(s *core.Step) { s.Mutant = "sum_empty_null" })})
		}
		if has["count"] {
			out = append(out, sensTarget{desc: name + " 的件数(LEFT JOIN 的行数: 空集合 = 1)", mutate: with(i, func(s *core.Step) { s.Mutant = "count_outer" })})
		}
		if has["min"] || has["max"] {
			out = append(out, sensTarget{desc: name + " 的最小 ↔ 最大", mutate: with(i, func(s *core.Step) {
				for k := range s.Aggs {
					switch s.Aggs[k].Func {
					case "min":
						s.Aggs[k].Func = "max"
					case "max":
						s.Aggs[k].Func = "min"
					}
				}
			})})
		}
	}
	return out
}

// sortedTables: 表按外键的依赖排序(被引用的表在前)。准备数据按这个顺序插入(子表的外键指向父表)。
func sortedTables(refs map[string][]string, tables []string) []string {
	sort.Strings(tables)
	in := map[string]bool{}
	for _, t := range tables {
		in[t] = true
	}
	var out []string
	done := map[string]bool{}
	var visit func(t string, depth int)
	visit = func(t string, depth int) {
		if done[t] || depth > len(tables) {
			return
		}
		for _, r := range refs[t] {
			if in[r] && r != t {
				visit(r, depth+1)
			}
		}
		if !done[t] {
			done[t] = true
			out = append(out, t)
		}
	}
	for _, t := range tables {
		visit(t, 0)
	}
	return out
}

// reCheckRegex: CHECK 里的正则(`列 ~ '…'`)。Postgres 的正则和 RE2 在常见的写法上一致;生成的样本仍然交给数据库判定。
var reCheckRegex = regexp.MustCompile(`~\*?\s*'((?:[^']|'')*)'`)

// regexSample: 符合正则的一个字符串(seed 不同 → 一般不同)。不支持的写法 → ""(候选值由数据库判定,不通过就不用)。
func regexSample(pattern string, seed int) string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return ""
	}
	rnd := rand.New(rand.NewSource(int64(seed)*7919 + 1))
	var b strings.Builder
	var gen func(r *syntax.Regexp) bool
	gen = func(r *syntax.Regexp) bool {
		switch r.Op {
		case syntax.OpLiteral:
			b.WriteString(string(r.Rune))
		case syntax.OpCharClass:
			if len(r.Rune) < 2 {
				return false
			}
			k := rnd.Intn(len(r.Rune) / 2)
			lo, hi := r.Rune[2*k], r.Rune[2*k+1]
			if hi-lo > 1000 {
				hi = lo + 1000
			}
			b.WriteRune(lo + rune(rnd.Intn(int(hi-lo)+1)))
		case syntax.OpAnyCharNotNL, syntax.OpAnyChar:
			b.WriteRune(rune('A' + rnd.Intn(26)))
		case syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText, syntax.OpEmptyMatch:
		case syntax.OpCapture:
			return gen(r.Sub[0])
		case syntax.OpConcat:
			for _, s := range r.Sub {
				if !gen(s) {
					return false
				}
			}
		case syntax.OpAlternate:
			return gen(r.Sub[rnd.Intn(len(r.Sub))])
		case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
			lo, hi := 0, 3
			switch r.Op {
			case syntax.OpPlus:
				lo = 1
			case syntax.OpQuest:
				hi = 1
			case syntax.OpRepeat:
				lo, hi = r.Min, r.Max
				if hi < 0 || hi > lo+3 {
					hi = lo + 3
				}
			}
			n := lo + rnd.Intn(hi-lo+1)
			for i := 0; i < n; i++ {
				if !gen(r.Sub[0]) {
					return false
				}
			}
		default:
			return false
		}
		return true
	}
	if !gen(re) {
		return ""
	}
	return b.String()
}
