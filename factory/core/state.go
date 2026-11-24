package core

// 当前状态表(docs/design/current-state.md): JSON版本 的"每键最新"的物化。
//
// 读(只用于生成 SQL;参考求值器跑原始流水线,差分测试因此同时验证触发器和这个改写):
//
//	scan(原始表) → project(展开) → filter(键非空)… → pick_per_key(K, 登记顺序; State = S) → 后面的步骤
//	  ⇒ scan(S) → project(展开) → filter(键非空)… → 后面的步骤
//
// 理由: S 由触发器维护,任何时刻 S 的行 = 原始表里 键非空 的行按 K 取登记顺序最新的一行(原样的一整行;墓碑也在)。
// 展开是逐行的,所以 展开(S) = pick_per_key(展开(原始表))。之后的筛选提到展开的后面(只越过筛选、只改名的 project 和 lookup):
// 没有了取最新,筛选之间、筛选和改名之间都可以交换;keyindex 因此能给 JSON 字段的条件加上索引的式子。
//
// 写(StateDDL): 状态表 + 触发器 + 索引 + 回填,都从同一段来源的流水线(展开 / 键非空 / 取最新)生成:
// 触发器里的"新的行的最新"= 同一条流水线换成转换表(REFERENCING NEW TABLE)的 SQL。

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// statePrefix: 流水线开头的 scan → project → filter… → pick_per_key(State) 的 pick_per_key 的位置;不是这个形状 = -1。
func statePrefix(steps []Step) int {
	if len(steps) < 3 || steps[0].Kind != "scan" || steps[1].Kind != "project" {
		return -1
	}
	for i := 2; i < len(steps); i++ {
		switch steps[i].Kind {
		case "filter":
			continue
		case "pick_per_key":
			if steps[i].State != "" {
				return i
			}
		}
		return -1
	}
	return -1
}

// stateRead: 见文件开头。不是那个形状时返回原来的流水线(读原始表: 慢,但对)。
func stateRead(p *Pipeline) *Pipeline {
	k := statePrefix(p.Steps)
	if k < 0 {
		return p
	}
	steps := append([]Step(nil), p.Steps[:k]...)
	steps[0].Table = p.Steps[k].State
	rest := append([]Step(nil), p.Steps[k+1:]...)
	hoisted, rest := hoistFilters(p.Steps[1], rest)
	out := append(append(append([]Step(nil), steps[:2]...), hoisted...), steps[2:]...)
	q := *p
	q.Steps = append(out, rest...)
	return &q
}

// hoistFilters: rest 开头的筛选(越过筛选和只改名的 project)改写成展开 project 的输出的列名,移出来。
func hoistFilters(expand Step, rest []Step) (hoisted, remain []Step) {
	name := map[string]string{}
	for _, x := range expand.Project {
		name[x.Name] = x.Name
	}
	for j, st := range rest {
		switch st.Kind {
		case "project":
			next := map[string]string{}
			for _, x := range st.Project {
				if x.Expr.Op != "col" {
					return hoisted, append(remain, rest[j:]...)
				}
				if src, ok := name[x.Expr.Name]; ok {
					next[x.Name] = src
				}
			}
			name = next
			remain = append(remain, st)
		case "lookup": // 和 pushdown 一样: lookup 只加列 / 逐行去掉找不到的行,不引用加的列的筛选可以越过
			for _, t := range st.Take {
				delete(name, t.Name)
			}
			remain = append(remain, st)
		case "filter":
			if e, ok := renameTo(st.Pred, name); ok {
				hoisted = append(hoisted, Step{Kind: "filter", Pred: e, From: st.From})
			} else {
				remain = append(remain, st)
			}
		default:
			return hoisted, append(remain, rest[j:]...)
		}
	}
	return hoisted, remain
}

// StateIndexCols: 流水线(和它的关联 / 嵌套 / 子表合计的来源)里读状态表时用到的非键的筛选列 / 关联列。
// 状态表名 → 列的组(每组一个索引;列名 = 来源展开后的列名)。
func StateIndexCols(p *Pipeline) map[string][][]string {
	out := map[string][][]string{}
	add := func(state string, cols []string) {
		key := strings.Join(cols, "\x00")
		for _, c := range out[state] {
			if strings.Join(c, "\x00") == key {
				return
			}
		}
		out[state] = append(out[state], cols)
	}
	var walk func(steps []Step)
	walk = func(steps []Step) {
		if k := statePrefix(steps); k >= 0 {
			state := steps[k].State
			hoisted, _ := hoistFilters(steps[1], steps[k+1:])
			for _, f := range hoisted {
				for _, c := range conjuncts(f.Pred) {
					if col := indexableCol(c); col != "" && !strings.HasPrefix(col, "_") {
						add(state, []string{col})
					}
				}
			}
		}
		for _, st := range steps {
			if len(st.Source) == 0 {
				continue
			}
			if k := statePrefix(st.Source); k >= 0 && len(st.On) > 0 {
				var cols []string
				for _, on := range st.On {
					cols = append(cols, on.Key)
				}
				// 对方的条件里的列(label_code = 'SHELF' 一类)接在关联的列后面
				if st.Pred != nil {
					for _, c := range conjuncts(st.Pred) {
						if col := indexableCol(c); col != "" && !hasStr(cols, col) {
							cols = append(cols, col)
						}
					}
				}
				// 子行的顺序(nest: 按顺序取前 N 行)接在最后: 按关联的列定位之后顺着索引读,读够就停。"^" = 文本按字节(COLLATE "C")
				for _, o := range st.Order {
					if !o.Desc && !hasStr(cols, o.Col) {
						cols = append(cols, "^"+o.Col)
					}
				}
				add(st.Source[k].State, cols)
			}
			walk(st.Source)
		}
	}
	walk(p.Steps)
	for _, v := range out {
		sort.Slice(v, func(i, j int) bool { return strings.Join(v[i], ",") < strings.Join(v[j], ",") })
	}
	return out
}

// indexableCol: 能用索引的条件(cmp(列, 不引用列的) / in_list(列) / or(absent, 这些))的列;不是 = ""。
func indexableCol(c *Expr) string {
	if c.Op == "or" && len(c.Args) == 2 && c.Args[0].Op == "absent" {
		return indexableCol(c.Args[1])
	}
	free := func(x *Expr) bool { return x.Op == "param" || x.Op == "const" || x.Op == "sql_ref" }
	switch {
	case c.Op == "cmp" && keyCmpOps[c.Name] && len(c.Args) == 2:
		if c.Args[0].Op == "col" && free(c.Args[1]) {
			return c.Args[0].Name
		}
		if c.Args[1].Op == "col" && free(c.Args[0]) {
			return c.Args[1].Name
		}
	case c.Op == "in_list" && len(c.Args) == 1 && c.Args[0].Op == "col":
		return c.Args[0].Name
	}
	return ""
}

func hasStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// StateDDL: 状态表的 DDL(幂等: 对已有的库执行 = 迁移,含回填)。source = 来源的流水线(到 pick_per_key 为止或更长),
// index = 读它的 API 用到的列的组(StateIndexCols)。
func StateDDL(source []Step, index [][]string) (string, error) {
	k := statePrefix(source)
	if k < 0 {
		return "", fmt.Errorf("状态表: 来源不是 scan → project → filter… → pick_per_key(State) 的形状")
	}
	scan, expand, pick := source[0], source[1], source[k]
	S, raw := pick.State, scan.Table
	def := map[string]*Expr{}
	for _, x := range expand.Project {
		def[x.Name] = x.Expr
	}
	var rawCols, keyCols []string
	for _, c := range scan.Columns {
		rawCols = append(rawCols, c.Name)
	}
	for _, kc := range pick.Keys {
		keyCols = append(keyCols, "_k_"+kc)
	}
	// 新的一行 / 原始表 → 每键最新的一行(原始表的列 + 键)
	selectFrom := func(table string) (string, Schema, error) {
		sc := scan
		sc.Table = table
		// 只展开键 / 登记顺序 / 键非空的条件用到的列: 其他字段的值不合法也不影响写入(和读一样,只在读到时校验)
		need := map[string]bool{}
		for _, kc := range pick.Keys {
			need[kc] = true
		}
		for _, o := range pick.Order {
			need[o.Col] = true
		}
		for _, f := range source[2:k] {
			for _, c := range exprCols(f.Pred) {
				need[c] = true
			}
		}
		ex := Step{Kind: "project", From: expand.From}
		for _, x := range expand.Project {
			if need[x.Name] {
				ex.Project = append(ex.Project, x)
			}
		}
		for _, c := range rawCols {
			ex.Project = append(ex.Project, NamedExpr{"_r_" + c, Col(c)})
		}
		steps := []Step{sc, ex}
		steps = append(steps, source[2:k]...)
		pk := pick
		pk.State = ""
		steps = append(steps, pk)
		var fin []NamedExpr
		for _, c := range rawCols {
			fin = append(fin, NamedExpr{c, Col("_r_" + c)})
		}
		for i, kc := range pick.Keys {
			fin = append(fin, NamedExpr{keyCols[i], Col(kc)})
		}
		steps = append(steps, Step{Kind: "project", Project: fin})
		q, err := ToSQL(&Pipeline{Steps: steps}, Options{})
		if err != nil {
			return "", nil, err
		}
		return q.Text, q.Columns, nil
	}
	fromRaw, cols, err := selectFrom(raw)
	if err != nil {
		return "", fmt.Errorf("状态表 %s: %w", S, err)
	}
	fromNew, _, err := selectFrom("factory_new")
	if err != nil {
		return "", err
	}
	fromOld, _, err := selectFrom("factory_old")
	if err != nil {
		return "", err
	}
	// 登记顺序: 新的一行(EXCLUDED)排在状态表的行(s)前面 = 更新
	var order []string
	for _, o := range pick.Order {
		e := def[o.Col]
		if e == nil || e.Op != "col" {
			return "", fmt.Errorf("状态表 %s: 登记顺序 %s 必须是原始表的列", S, o.Col)
		}
		t, _ := Schema(scan.Columns).Col(e.Name)
		a, b := "EXCLUDED."+quoteIdent(e.Name), "s."+quoteIdent(e.Name)
		ca, cb := a, b
		if t.Kind == Text {
			ca, cb = a+` COLLATE "C"`, b+` COLLATE "C"`
		}
		// NULL 最大(ASC NULLS LAST / DESC NULLS FIRST,和 pick_per_key 一样)
		var before string
		if o.Desc {
			before = fmt.Sprintf("((%s IS NULL AND %s IS NOT NULL) OR %s > %s)", a, b, ca, cb)
		} else {
			before = fmt.Sprintf("((%s IS NOT NULL AND %s IS NULL) OR %s < %s)", a, b, ca, cb)
		}
		order = append(order, before, fmt.Sprintf("%s IS NOT DISTINCT FROM %s", a, b))
	}
	newer := order[len(order)-2]
	for i := len(order) - 4; i >= 0; i -= 2 {
		newer = fmt.Sprintf("(%s OR (%s AND %s))", order[i], order[i+1], newer)
	}

	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	q := quoteIdent
	all := append(append([]string(nil), rawCols...), keyCols...)
	qall := make([]string, len(all))
	for i, c := range all {
		qall[i] = q(c)
	}
	qkeys := make([]string, len(keyCols))
	for i, c := range keyCols {
		qkeys[i] = q(c)
	}
	var set []string
	for _, c := range rawCols {
		set = append(set, fmt.Sprintf("%s = EXCLUDED.%s", q(c), q(c)))
	}
	w("-- 当前状态表: %s(逻辑表的每个键一行 = 原始表 %s 里登记顺序最新的一行,含删除的墓碑)。\n", S, raw)
	w("-- 由工厂生成(docs/design/current-state.md),不要手改;触发器在写原始表的同一个事务里维护它。\n")
	w("CREATE TABLE IF NOT EXISTS %s (\n", q(S))
	var defs []string
	for _, c := range cols {
		t := c.Type.Kind.SQLType()
		if c.Type.Kind == Text && strings.HasPrefix(c.Name, "_k_") {
			t += ` COLLATE "C"`
		}
		defs = append(defs, fmt.Sprintf("    %s %s", q(c.Name), t))
	}
	defs = append(defs, fmt.Sprintf("    CONSTRAINT %s UNIQUE NULLS NOT DISTINCT (%s)", q(ident(S, "key")), strings.Join(qkeys, ", ")))
	w("%s\n);\n", strings.Join(defs, ",\n"))
	// 索引: 自然键(等值;文本另加 COLLATE "C" 的: 按键排序 / 分页)+ API 用到的列
	expr := func(col string) (string, Kind, error) {
		e := def[col]
		if e == nil {
			return "", "", fmt.Errorf("状态表 %s: 列 %s 不在展开里", S, col)
		}
		if e.Op == "col" {
			t, _ := Schema(scan.Columns).Col(e.Name)
			return q(e.Name), t.Kind, nil
		}
		if e.Op == "json_field" && len(e.Args) == 1 && e.Args[0].Op == "col" {
			s, ok := JSONKeyIndexSQL(e.Args[0].Name, e.Name, e.Type.Kind)
			if !ok {
				return "", "", fmt.Errorf("状态表 %s: 列 %s(%s)不能做索引的键", S, col, e.Type.Kind)
			}
			return s, e.Type.Kind, nil
		}
		return "", "", fmt.Errorf("状态表 %s: 列 %s 的式子不能做索引", S, col)
	}
	idx := func(suffix string, cols []string, collateC bool) error {
		var es []string
		for _, c := range cols {
			byteOrder := strings.HasPrefix(c, "^")
			s, kind, err := expr(strings.TrimPrefix(c, "^"))
			if err != nil {
				return err
			}
			if (collateC || byteOrder) && kind == Text {
				s = "(" + s + ` COLLATE "C")`
			}
			es = append(es, s)
		}
		w("CREATE INDEX IF NOT EXISTS %s ON %s (%s);\n", q(ident(S, suffix)), q(S), strings.Join(es, ", "))
		return nil
	}
	if err := idx("key_idx", pick.Keys, false); err != nil {
		return "", err
	}
	hasText := false
	for _, kc := range pick.Keys {
		if _, kind, _ := expr(kc); kind == Text {
			hasText = true
		}
	}
	if hasText {
		if err := idx("key_c_idx", pick.Keys, true); err != nil {
			return "", err
		}
	}
	for _, cs := range index {
		if strings.Join(cs, ",") == strings.Join(pick.Keys, ",") {
			continue
		}
		if err := idx(strings.ReplaceAll(strings.Join(cs, "_"), "^", "")+"_idx", cs, false); err != nil {
			return "", err
		}
	}
	upsert := func(from string) string {
		return fmt.Sprintf("INSERT INTO %s AS s (%s)\nSELECT * FROM (\n%s\n) AS v\nON CONFLICT (%s) DO UPDATE SET %s\nWHERE %s",
			q(S), strings.Join(qall, ", "), indent(from), strings.Join(qkeys, ", "), strings.Join(set, ", "), newer)
	}
	// 按键重算(UPDATE / DELETE 原始表: 数据修复): 受影响的键先删掉,再从原始表取最新
	var match []string
	for _, c := range qkeys {
		match = append(match, fmt.Sprintf("s.%s IS NOT DISTINCT FROM a.%s", c, c))
	}
	affected := func(froms ...string) string {
		var parts []string
		for _, f := range froms {
			parts = append(parts, fmt.Sprintf("SELECT %s FROM (\n%s\n) AS x", strings.Join(qkeys, ", "), indent(f)))
		}
		return strings.Join(parts, "\nUNION ALL\n")
	}
	recompute := func(froms ...string) string {
		aff := affected(froms...)
		return fmt.Sprintf("DELETE FROM %s AS s WHERE EXISTS (SELECT 1 FROM (\n%s\n) AS a WHERE %s);\n"+
			"INSERT INTO %s (%s)\nSELECT s.* FROM (\n%s\n) AS s WHERE EXISTS (SELECT 1 FROM (\n%s\n) AS a WHERE %s)\nON CONFLICT DO NOTHING;",
			q(S), indent(aff), strings.Join(match, " AND "), q(S), strings.Join(qall, ", "), indent(fromRaw), indent(aff), strings.Join(match, " AND "))
	}
	fn := func(suffix, body string) {
		w("CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $factory$\nBEGIN\n%s\nRETURN NULL;\nEND\n$factory$;\n",
			q(ident(S, suffix)), indent(body))
	}
	fn("ins", upsert(fromNew)+";")
	fn("upd", recompute(fromOld, fromNew))
	fn("del", recompute(fromOld))
	fn("trunc", fmt.Sprintf("TRUNCATE %s;", q(S)))
	for _, t := range []struct{ suffix, when string }{
		{"ins", "AFTER INSERT ON %[1]s REFERENCING NEW TABLE AS factory_new"},
		{"upd", "AFTER UPDATE ON %[1]s REFERENCING OLD TABLE AS factory_old NEW TABLE AS factory_new"},
		{"del", "AFTER DELETE ON %[1]s REFERENCING OLD TABLE AS factory_old"},
		{"trunc", "AFTER TRUNCATE ON %[1]s"},
	} {
		name := q(ident(S, t.suffix))
		w("CREATE OR REPLACE TRIGGER %s %s FOR EACH STATEMENT EXECUTE FUNCTION %s();\n", name, fmt.Sprintf(t.when, q(raw)), name)
	}
	w("-- 回填(已有的数据;幂等)\n%s;\n", upsert(fromRaw))
	return b.String(), nil
}

// ident: 状态表的附属对象名(63 字节以内: 太长时缩短并加哈希)。
func ident(table, suffix string) string {
	s := table + "_" + suffix
	if len(s) <= 63 {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%s_%x", s[:63-9], h[:4])
}

// exprCols: 表达式引用的列名。
func exprCols(e *Expr) []string {
	if e == nil {
		return nil
	}
	if e.Op == "col" {
		return []string{e.Name}
	}
	var out []string
	for _, a := range e.Args {
		out = append(out, exprCols(a)...)
	}
	return out
}
