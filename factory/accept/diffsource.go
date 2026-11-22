package accept

// 差分测试的多表: 关联 / 嵌套 / 子聚合的对方是逻辑表(来源单元展开的表: JSON版本 等)。
//
// 对方的数据按逻辑行的"版本"造: 每个自然键 1~3 个版本(旧版本的值和新版本不同),一部分键的最新版本是删除 / 逻辑删除,
// 一部分键删除之后又登记(复活),可以为空的键列约 1/5 为 NULL,偶尔有键为空的版本(来源取数时丢掉)。
// 版本按顺序写进物理表(和例子的 准备数据 一样: 行的顺序 = 登记的顺序;loadStorageRows),读回物理表的行交给参考求值器,
// 参考求值器按来源的子流水线(Step.Source)算出逻辑表 —— 和 SQL 后端各算各的。
//
// 外键的值(关联条件推出来的「当前行的列 → 对方的键」)从**全部的版本**里取: 最新是删除的键、只在旧版本里出现的值都会被引用到,
// "找到了已经删除的键"/"按旧版本关联"的错误实现才看得出来。
//
// 覆盖条件(sensTargets: 错误实现 → ≥3 个请求能区分):
//   - 取最旧的版本(取最新的顺序反过来)
//   - 不处理删除 / 逻辑删除(去掉来源的有效条件)
//   - 按物理表的行关联(不取最新: 每个版本都关联,删除的行另外去掉)

import (
	"fmt"
	"math/big"
	"strings"
	"time"

	"apifactory/factory/constraints"
	"apifactory/factory/core"
	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// logicalTable: 关联 / 子表是逻辑表时的数据(otherTable.lt / childTable.lt)。
type logicalTable struct {
	view     string           // 逻辑表名
	st       *ir.Storage      // 物理存储
	source   []core.Step      // 来源的子流水线(参考求值器算逻辑表)
	physCols []core.NamedType // 物理表的列(读回)
	cols     []core.NamedType // 逻辑表的列
	versions []core.Row       // 生成的全部版本(逻辑列的值;外键从这里取值)
	current  []core.Row       // 逻辑表现在的行(参考求值器按来源算的)
	phys     []core.Row       // 物理表读回的行(参考求值器的输入)
}

func (e *diffEnv) newLogical(st core.Step) *logicalTable {
	if len(st.Source) == 0 || st.Source[0].Kind != "scan" {
		return nil
	}
	return &logicalTable{view: st.Table, st: e.plan.Storage(st.Table), source: st.Source, physCols: st.Source[0].Columns, cols: st.Columns}
}

// version: 一个版本(逻辑行)。key = 自然键的值;action = "" / 删除 / 逻辑删除。
type version struct {
	row    core.Row
	action string
}

// genValue: 逻辑表的一列(不是键)的值: 类型的小值域(和普通的关联的表一样)。
func (e *diffEnv) genValue(c core.NamedType, i int) any {
	return e.otherValue(c, i, false)
}

// shellCol: 外壳列(物理表的列,不在 JSON 里;值由登记顺序合成,不生成)。
func (lt *logicalTable) shellCol(name string) bool {
	if lt.st == nil {
		return false
	}
	for _, f := range lt.st.Fields {
		if f.Name == name && f.Column != "" {
			return true
		}
	}
	return false
}

// keyNullable: 自然键的列可以为空(领域里 可以为空: 是)。
func (lt *logicalTable) keyNullable(col string) bool {
	for _, c := range lt.cols {
		if c.Name == col {
			return c.Type.Nullable
		}
	}
	return false
}

// versionsFor: 一个键的版本: 1~3 个(值各不相同),最后一个约 1/4 删除、约 1/6 逻辑删除;约 1/8 中间删除后又登记。
// fill(row, j): 填这个版本的非键列(外键等)。
func (e *diffEnv) versionsFor(lt *logicalTable, key core.Row, fill func(core.Row, int)) []version {
	n := 1 + e.rnd.Intn(3)
	var out []version
	for j := 0; j < n; j++ {
		r := core.Row{}
		for k, v := range key {
			r[k] = v
		}
		for _, c := range lt.cols {
			if _, isKey := key[c.Name]; isKey || lt.shellCol(c.Name) {
				continue
			}
			r[c.Name] = e.genValue(c, j)
		}
		if fill != nil {
			fill(r, j)
		}
		out = append(out, version{row: r})
		if j == 0 && n > 1 && e.rnd.Intn(8) == 0 { // 删除之后又登记(复活: 最新的版本有效)
			out = append(out, version{row: key, action: spec.ActionDelete})
		}
	}
	switch r := e.rnd.Intn(24); { // 约 1/4 删除、约 1/6 逻辑删除(链的末端的表只有经过前面的关联才看得到,删除要多一些)
	case r < 6: // 删除的版本也带着最后的值(只看操作列才知道是删除: 不处理删除的实现会把这些值读出来)
		out = append(out, version{row: out[len(out)-1].row, action: spec.ActionDelete})
	case r < 10 && lt.st != nil && lt.st.LogicalDeleteKey != "":
		last := out[len(out)-1].row
		out = append(out, version{row: last, action: spec.ActionLogicalDelete})
	}
	return out
}

// loadLogical: 版本写进物理表(行的顺序 = 登记的顺序),读回,按来源算出逻辑表。
func (e *diffEnv) loadLogical(lt *logicalTable, vs []version) error {
	if lt.st == nil {
		return fmt.Errorf("逻辑表 %s: 没有存储的定义", lt.view)
	}
	types := core.Schema(lt.cols)
	var rows []map[string]any
	lt.versions = nil
	for _, v := range vs {
		m := map[string]any{}
		for k, x := range v.row {
			t, _ := types.Col(k)
			m[k] = jsonFriendly(x, t.Kind)
		}
		if v.action != "" {
			m[spec.FixtureAction] = v.action
		}
		rows = append(rows, m)
		lt.versions = append(lt.versions, v.row)
	}
	if err := loadStorageRows(e.ctx, e.db, e.plan, lt.st, rows); err != nil {
		return fmt.Errorf("逻辑表 %s: %w", lt.view, err)
	}
	var err error
	if lt.phys, _, err = e.readBack(lt.st.Table, lt.physCols, nil); err != nil {
		return err
	}
	res, err := core.Eval(&core.Pipeline{Steps: lt.source}, map[string][]core.Row{lt.st.Table: lt.phys}, nil)
	if err != nil {
		return fmt.Errorf("逻辑表 %s 的来源: %w", lt.view, err)
	}
	lt.current = res.Rows
	return nil
}

// jsonFriendly: 逻辑行的值 → JSON 里的写法(十进制 = 文本,日期 = YYYY-MM-DD,时间 = RFC 3339)。
func jsonFriendly(v any, k core.Kind) any {
	switch x := v.(type) {
	case *big.Rat:
		return core.RatString(x)
	case time.Time:
		if k == core.Date {
			return x.Format("2006-01-02")
		}
		return x.UTC().Format(time.RFC3339Nano)
	}
	return v
}

// loadOtherLogical: 关联的对方是逻辑表: 键 = 序号的值(多列的键: 每列小值域),每个键几个版本,外键在每个版本里各自取值。
func (e *diffEnv) loadOtherLogical(ot *otherTable) error {
	lt := ot.lt
	nk := lt.st.NaturalKey
	n := diffRows / 2 * max(e.scale, 1)
	var vs []version
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		key := core.Row{}
		for _, k := range nk {
			c, _ := core.Schema(lt.cols).Col(k)
			switch {
			case lt.keyNullable(k) && e.rnd.Intn(5) == 0:
				key[k] = nil
			case e.refDomain(ot, k) != nil: // 引用这一列的列有值域(请求的格式约束): 键在值域里取,请求才合法
				d := e.refDomain(ot, k)
				if d.Expand != nil {
					key[k] = d.Expand(3000 + i)
				} else {
					key[k] = d.Values[e.rnd.Intn(len(d.Values))]
				}
			case len(nk) == 1:
				key[k] = e.scalar(c.Kind, true, i)
			default:
				key[k] = e.scalar(c.Kind, false, i)
			}
		}
		ks := keyText(key, nk)
		if seen[ks] {
			continue
		}
		seen[ks] = true
		vs = append(vs, e.versionsFor(lt, key, func(r core.Row, j int) {
			e.applyFKs(lt.view, lt.cols, r, nil, i*3+j)
		})...)
	}
	if e.rnd.Intn(2) == 0 { // 键为空的版本(取数时丢掉;不能被关联到)
		key := core.Row{}
		for _, k := range nk {
			key[k] = nil
		}
		vs = append(vs, version{row: key})
	}
	if err := e.loadLogical(lt, vs); err != nil {
		return err
	}
	ot.rows = lt.versions // 外键的值从全部的版本里取(删除了的键、旧版本的值)
	return nil
}

// loadChildLogical: 子表是逻辑表: 每个父行 childCount 个有效的子行(键 = 父行的值 + 父行内的序号),
// 每个子行 1~3 个版本;另外约 1/3 的父行多一个最新是删除的子行(有效的子行数不变),几个关联不到父行的子行。
func (e *diffEnv) loadChildLogical(ct *childTable, parents []core.Row) error {
	lt := ct.lt
	nk := lt.st.NaturalKey
	fk := map[string]int{}
	for i, c := range ct.fkCols {
		fk[c] = i
	}
	var vs []version
	seq := 0
	mkKey := func(parent core.Row, j int) core.Row {
		key := core.Row{}
		for _, k := range nk {
			c, _ := core.Schema(lt.cols).Col(k)
			if i, ok := fk[k]; ok {
				if parent == nil {
					key[k] = missingKey(c.Kind, j)
				} else {
					key[k] = parent[ct.pkCols[i]]
				}
				continue
			}
			if lt.keyNullable(k) && j == 0 && e.rnd.Intn(3) == 0 { // 可以为空的键: 空值自己是一个子行
				key[k] = nil
				continue
			}
			key[k] = e.scalar(c.Kind, true, j)
		}
		return key
	}
	fill := func(parent core.Row) func(core.Row, int) {
		return func(r core.Row, _ int) {
			for i, c := range ct.fkCols { // 子行的非键的外键列(键里没有关联列时)
				if _, isKey := r[c]; !isKey || parent == nil {
					continue
				}
				r[c] = parent[ct.pkCols[i]]
			}
		}
	}
	live := func(key core.Row, f func(core.Row, int)) []version { // 最新的版本有效
		var out []version
		for {
			out = e.versionsFor(lt, key, f)
			if out[len(out)-1].action == "" {
				return out
			}
		}
	}
	for _, parent := range parents {
		skip := false
		for _, c := range ct.pkCols {
			skip = skip || parent[c] == nil
		}
		if skip {
			continue
		}
		n := e.childCount(ct.max)
		for j := 0; j < n; j++ {
			vs = append(vs, live(mkKey(parent, j), fill(parent))...)
			seq++
		}
		if e.rnd.Intn(3) == 0 { // 最新是删除 / 逻辑删除的子行(不算在子行里)
			key := mkKey(parent, n+1)
			v := e.versionsFor(lt, key, fill(parent))
			if v[len(v)-1].action == "" {
				action := spec.ActionDelete
				if lt.st.LogicalDeleteKey != "" && e.rnd.Intn(2) == 0 {
					action = spec.ActionLogicalDelete
				}
				v = append(v, version{row: v[len(v)-1].row, action: action})
			}
			vs = append(vs, v...)
		}
	}
	for j := 0; j < 3; j++ { // 关联不到父行的子行
		vs = append(vs, live(mkKey(nil, j), nil)...)
	}
	if err := e.loadLogical(lt, vs); err != nil {
		return err
	}
	ct.rows = lt.current
	return nil
}

// mainLogicalRows: 数据来源是逻辑表时,来源部分算出来的逻辑行(子表的父行)。
func (e *diffEnv) mainLogicalRows() []core.Row {
	var src []core.Step
	for _, st := range e.api.Pipeline.Steps {
		if st.Kind == "scan" || strings.HasPrefix(st.From, "来源 ") {
			src = append(src, st)
		}
	}
	res, err := core.Eval(&core.Pipeline{Steps: src, Params: e.api.Pipeline.Params}, map[string][]core.Row{e.api.Pipeline.Steps[0].Table: e.rows}, nil) // e.table 在造子表时会临时换掉
	if err != nil {
		return nil
	}
	return res.Rows
}

// applyMainFKs: 数据来源是逻辑表(JSON 列)时,这一行的外键(逻辑列 → 对方的键)写进 JSON 对象。
func (e *diffEnv) applyMainFKs(jsonCol string, obj map[string]any, i int) {
	fks := e.fks[e.api.Table]
	if len(fks) == 0 || e.plan.Storage(e.api.Table) == nil {
		return
	}
	m := core.Row{}
	for _, fk := range fks {
		for _, c := range fk.cols {
			if k := e.jsonKeyOf(jsonCol, c); k != "" {
				m[c] = jsonScalar(obj[k])
			}
		}
	}
	e.applyFKs(e.api.Table, e.mainLogical, m, nil, i)
	for c, v := range m {
		if k := e.jsonKeyOf(jsonCol, c); k != "" {
			t, _ := e.mainLogical.Col(c)
			obj[k] = jsonFriendly(v, t.Kind)
		}
	}
}

// sourceTargets: 对方是逻辑表时的覆盖条件(第 i 步: lookup / nest / child_agg)。
func sourceTargets(i int, st core.Step, name string) []sensTarget {
	if len(st.Source) == 0 {
		return nil
	}
	with := func(f func(src []core.Step) []core.Step) func(p *core.Pipeline) *core.Pipeline {
		return func(p *core.Pipeline) *core.Pipeline {
			mut := *p
			mut.Steps = append([]core.Step(nil), p.Steps...)
			s := mut.Steps[i]
			s.Source = f(append([]core.Step(nil), s.Source...))
			mut.Steps[i] = s
			return &mut
		}
	}
	pick, valid := -1, -1
	for j, s := range st.Source {
		if s.Kind == "pick_per_key" {
			pick = j
		}
		if s.Kind == "filter" && pick >= 0 && j > pick && valid < 0 {
			valid = j
		}
	}
	var out []sensTarget
	if pick >= 0 {
		out = append(out, sensTarget{desc: name + " 的逻辑表: 取了最旧的版本", mutate: with(func(src []core.Step) []core.Step {
			s := src[pick]
			s.Order = append([]core.SortKey(nil), s.Order...)
			for k := range s.Order {
				s.Order[k].Desc = !s.Order[k].Desc
			}
			src[pick] = s
			return src
		})})
	}
	if valid >= 0 {
		out = append(out, sensTarget{desc: name + " 的逻辑表: 不处理删除 / 逻辑删除", mutate: with(func(src []core.Step) []core.Step {
			return append(src[:valid], src[valid+1:]...)
		})})
		// 有效条件的每一项单独去掉(只漏了删除 / 只漏了逻辑删除的实现)。不引用列的项(没有逻辑删除时的恒真项)不要求
		if pred := st.Source[valid].Pred; pred != nil && pred.Op == "and" {
			for j := range pred.Args {
				j := j
				if !refersColumn(pred.Args[j]) {
					continue
				}
				what := []string{"删除", "逻辑删除"}[min(j, 1)]
				out = append(out, sensTarget{desc: fmt.Sprintf("%s 的逻辑表: 不处理%s", name, what), mutate: with(func(src []core.Step) []core.Step {
					s := src[valid]
					var rest []*core.Expr
					for k, a := range s.Pred.Args {
						if k != j {
							rest = append(rest, a)
						}
					}
					if len(rest) == 1 {
						s.Pred = rest[0]
					} else {
						s.Pred = core.And(rest...)
					}
					src[valid] = s
					return src
				})})
			}
		}
	}
	if pick >= 0 && valid >= 0 {
		out = append(out, sensTarget{desc: name + " 的逻辑表: 按物理表的行关联(不取最新)", mutate: with(func(src []core.Step) []core.Step {
			return append(src[:pick], src[pick+1:]...)
		})})
	}
	return out
}

// refDomain: 引用逻辑表 ot 的键列 key 的列(来源的表的外键)的值域(筛选参数的约束)。没有 → nil。
func (e *diffEnv) refDomain(ot *otherTable, key string) *constraints.Domain {
	for _, fks := range e.fks {
		for _, fk := range fks {
			if fk.ref != ot {
				continue
			}
			for j, k := range fk.keys {
				if d, ok := e.domains[fk.cols[j]]; ok && k == key && (d.Expand != nil || len(d.Values) > 0) {
					return d
				}
			}
		}
	}
	return nil
}
