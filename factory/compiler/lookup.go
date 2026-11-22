package compiler

// 多表: 关联(datasource.lookups → 核心的 lookup)。
//
//   - 关联的表必须在同一个库里(db/schema 里的普通的表,或 存储: JSON版本 等来源单元展开的逻辑表:
//     对方 = 来源的子流水线(每个自然键最新的一行,删除 / 逻辑删除去掉),唯一键 = 自然键)
//   - 分库的表只能关联同一个分库里的表: 两边都按分库键分库,关联条件里有「分库键 = 对方的分库键」
//   - 对方的键列必须是主键或唯一约束(唯一键从 db/schema 的 CREATE TABLE / CREATE UNIQUE INDEX 读)→ 核心的 DRC 检查
//   - 加的列之后和来源的列一样: 筛选、输出、固定条件、排序都可以用(列的目录 view 里加上这些列)

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"apifactory/factory/cells"
	"apifactory/factory/core"
	"apifactory/factory/sources"
	"apifactory/factory/spec"
	"apifactory/factory/where"
)

// lookupView: 关联的流水线步骤 + 加了关联列的目录(来源的表的列 + 每个关联加的列;keep 的列可空)。
// 之后接嵌套 / 子聚合(nest.go: 子行的列表 / 聚合值也加到目录里)。没有关联、嵌套、子聚合时返回原来的目录。
// lib: 单元库(逻辑表的来源单元的展开;nil = 只要目录,不要步骤: 设计编译器用)。
func lookupView(s *spec.Spec, cat *catalog, keys map[string][][]string, lib *cells.Library) (*catalog, []core.Step, error) {
	ls := s.Datasource.Lookups
	if len(ls) == 0 && len(s.Datasource.Nests) == 0 && len(s.Datasource.ChildAggs) == 0 {
		return cat, nil, nil
	}
	table := s.Datasource.Table
	view := &catalog{tables: map[string][]catColumn{}}
	for k, v := range cat.tables {
		view.tables[k] = v
	}
	cols := append([]catColumn(nil), cat.tables[table]...)
	entity := func(t string) spec.Entity {
		if s.Domain == nil {
			return spec.Entity{}
		}
		return s.Domain.Entities[t]
	}
	// 分库键: 来源的分库键 + 关联加的、来自对方的分库键的列(链式)
	shardCols := map[string]bool{}
	if sb := entity(table).ShardBy; sb != "" {
		shardCols[sb] = true
	}
	var steps []core.Step
	for _, l := range ls {
		st, next, err := lookupStep(s, cat, keys, lib, l, fmt.Sprintf("关联 %s", l.Name), table, cols, shardCols)
		if err != nil {
			return nil, nil, err
		}
		cols = next
		steps = append(steps, st)
	}
	cols, children, err := childSteps(s, cat, keys, cols, shardCols, lib)
	if err != nil {
		return nil, nil, err
	}
	view.tables[table] = cols
	return view, append(steps, children...), nil
}

// lookupStep: 一个关联 → 核心的 lookup 和加了列之后的当前行的列。table = 当前行的表(报错 / 分库用:
// 数据来源的表,或嵌套的子表);cols = 当前行的列(条件的左边);shardCols: 当前行里来自分库键的列(加的列也会登记)。
func lookupStep(s *spec.Spec, cat *catalog, keys map[string][][]string, lib *cells.Library, l spec.Lookup, at, table string, cols []catColumn, shardCols map[string]bool) (core.Step, []catColumn, error) {
	cols = append([]catColumn(nil), cols...)
	entity := func(t string) spec.Entity {
		if s.Domain == nil {
			return spec.Entity{}
		}
		return s.Domain.Entities[t]
	}
	st := core.Step{Kind: "lookup", Table: l.Table, From: at, Unique: keys[l.Table]}
	other, ok := cat.tables[l.Table]
	if !ok {
		return st, nil, fmt.Errorf("%s: 表 %q 不在 db/schema 里", at, l.Table)
	}
	if err := relatedSource(s, &st, cat, lib); err != nil { // 逻辑表: 来源的展开 + 唯一键 = 自然键
		return st, nil, fmt.Errorf("%s: %w", at, err)
	}
	switch l.Missing {
	case "", "keep":
		st.Missing = "keep"
	case "drop":
		st.Missing = "drop"
	default:
		return st, nil, fmt.Errorf("%s: missing 只能是 keep / drop,实际是 %q", at, l.Missing)
	}
	for _, c := range other {
		t, err := coreType(l.Table, c)
		if err != nil {
			return st, nil, fmt.Errorf("%s: %w", at, err)
		}
		st.Columns = append(st.Columns, core.NamedType{Name: c.Name, Type: t})
	}
	has := func(cs []catColumn, n string) bool {
		for _, c := range cs {
			if c.Name == n {
				return true
			}
		}
		return false
	}
	shardOK := false
	var preds []*core.Expr // 对方的列 = 常量(对方的行先按它筛)
	for _, on := range l.On {
		a, b, ok := strings.Cut(on, "=")
		a, b = strings.TrimSpace(a), strings.TrimSpace(b)
		if ok && isLiteral(b) {
			e, err := where.Parse(on, WhereTypes(other))
			if err != nil {
				return st, nil, fmt.Errorf("%s: 条件 %q(对方的列 = 常量): %w", at, on, err)
			}
			preds = append(preds, e)
			continue
		}
		if !ok || a == "" || b == "" || strings.Contains(b, "=") {
			return st, nil, fmt.Errorf("%s: 条件 %q 写成 \"当前行的列 = %s 的列\"", at, on, l.Table)
		}
		if !has(cols, a) {
			return st, nil, fmt.Errorf("%s: 条件 %q: %s 不是 %s 的列(也不是前面的关联加的列)", at, on, a, table)
		}
		if !has(other, b) {
			return st, nil, fmt.Errorf("%s: 条件 %q: %s 不是 %s 的列", at, on, b, l.Table)
		}
		if sb := entity(l.Table).ShardBy; sb != "" && b == sb && shardCols[a] {
			shardOK = true
		}
		st.On = append(st.On, core.JoinKey{Col: a, Key: b})
	}
	switch len(preds) {
	case 0:
	case 1:
		st.Pred = preds[0]
	default:
		st.Pred = core.And(preds...)
	}
	if len(st.On) == 0 {
		return st, nil, fmt.Errorf("%s: 条件里要有 \"当前行的列 = %s 的列\"(只有常量的条件不是关联)", at, l.Table)
	}
	if a := l.AsOf; a != nil {
		if !has(other, a.Start) {
			return st, nil, fmt.Errorf("%s: 按日期的开始 %s 不是 %s 的列", at, a.Start, l.Table)
		}
		if a.End != "" && !has(other, a.End) {
			return st, nil, fmt.Errorf("%s: 按日期的结束 %s 不是 %s 的列", at, a.End, l.Table)
		}
		var ref *core.Expr
		switch {
		case a.Ref == "今天":
			ref = core.Today(whereOpt(s, table).Zone)
		case a.Ref == "现在":
			ref = core.Now()
		case has(cols, a.Ref):
			ref = core.Col(a.Ref)
		default:
			return st, nil, fmt.Errorf("%s: 按日期的参照 %s 不是当前行的列(也不是 今天 / 现在)", at, a.Ref)
		}
		st.AsOf = &core.AsOf{Start: a.Start, End: a.End, EndIn: a.EndIn, Ref: ref}
	}
	onKeys := map[string]bool{}
	for c := range core.PinnedCols(st.Pred) {
		onKeys[c] = true
	}
	if st.AsOf != nil {
		onKeys[st.AsOf.Start] = true
	}
	var keyList []string
	for _, k := range st.On {
		onKeys[k.Key] = true
		keyList = append(keyList, k.Key)
	}
	unique := false
	for _, u := range st.Unique {
		all := true
		for _, c := range u {
			all = all && onKeys[c]
		}
		unique = unique || all
	}
	if !unique {
		return st, nil, fmt.Errorf("%s: 条件里 %s 的列 %v 不是主键或唯一约束(%s 的唯一键: %v)。"+
			"关联是多对一: 对方最多一行匹配;一对多请用嵌套", at, l.Table, keyList, l.Table, st.Unique)
	}
	switch src, dst := entity(table).ShardBy, entity(l.Table).ShardBy; {
	case src == "" && dst != "":
		return st, nil, fmt.Errorf("%s: %s 按 %s 分库,%s 不分库: 不分库的表不能关联分库的表(一个请求只访问一个库)", at, l.Table, dst, table)
	case src != "" && dst == "":
		return st, nil, fmt.Errorf("%s: %s 按 %s 分库,%s 不分库: 分库的表只能关联同一个分库里的表", at, table, src, l.Table)
	case src != "" && !shardOK:
		return st, nil, fmt.Errorf("%s: 分库的表之间的关联,条件里要有「%s = %s」(两边的分库键相等 → 对方的行在同一个库里)", at, src, dst)
	}
	for _, t := range l.Columns {
		var oc *catColumn
		for i := range other {
			if other[i].Name == t.From {
				oc = &other[i]
			}
		}
		if oc == nil {
			return st, nil, fmt.Errorf("%s: 列 %s: %s 没有列 %s", at, t.Name, l.Table, t.From)
		}
		if has(cols, t.Name) {
			return st, nil, fmt.Errorf("%s: 加的列 %s 和 %s 的列(或前面的关联加的列)重名", at, t.Name, table)
		}
		cols = append(cols, catColumn{Name: t.Name, Type: oc.Type, NotNull: oc.NotNull && st.Missing == "drop"})
		st.Take = append(st.Take, core.TakeCol{Name: t.Name, Col: t.From})
		if sb := entity(l.Table).ShardBy; sb != "" && t.From == sb {
			shardCols[t.Name] = true
		}
	}
	return st, cols, nil
}

// LookupColumns: 设计编译器用: 加了关联列之后的来源的列(检查筛选 / 输出 / 固定条件用)。
func LookupColumns(s *spec.Spec, tables map[string][]catColumn) ([]catColumn, error) {
	cat := &catalog{tables: tables}
	keys, err := UniqueKeys()
	if err != nil {
		return nil, err
	}
	view, _, err := lookupView(s, cat, keys, nil)
	if err != nil {
		return nil, err
	}
	return view.tables[s.Datasource.Table], nil
}

var (
	reComment     = regexp.MustCompile(`--[^\n]*`)
	reCreateTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?("?[A-Za-z_][A-Za-z0-9_]*"?)\s*\(`)
	reUniqueIndex = regexp.MustCompile(`(?is)CREATE\s+UNIQUE\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?[A-Za-z0-9_"]+\s+ON\s+(?:ONLY\s+)?("?[A-Za-z_][A-Za-z0-9_]*"?)\s*(?:USING\s+\w+\s*)?\(([^()]*)\)\s*([^;]*);`)
	reTableKey    = regexp.MustCompile(`(?is)^(?:CONSTRAINT\s+\S+\s+)?(PRIMARY\s+KEY|UNIQUE)\s*(?:NULLS\s+(?:NOT\s+)?DISTINCT\s*)?\(([^()]*)\)`)
	reColKey      = regexp.MustCompile(`(?i)\b(PRIMARY\s+KEY|UNIQUE)\b`)
	reIdent       = regexp.MustCompile(`^"?[A-Za-z_][A-Za-z0-9_]*"?$`)
)

// UniqueKeys: db/schema/*.sql 里每张表的唯一键(主键 / 唯一约束 / 没有条件的唯一索引;列的组合,按书写顺序)。
// 只认简单的写法(表达式索引、部分索引不算: 不能保证"最多一行匹配")。
func UniqueKeys() (map[string][][]string, error) {
	files, err := filepath.Glob("db/schema/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	out := map[string][][]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		parseUniqueKeys(string(b), out)
	}
	return out, nil
}

func parseUniqueKeys(src string, out map[string][][]string) {
	src = reComment.ReplaceAllString(src, "")
	unq := func(s string) string { return strings.Trim(strings.TrimSpace(s), `"`) }
	list := func(s string) ([]string, bool) {
		var cols []string
		for _, c := range strings.Split(s, ",") {
			c = strings.TrimSpace(c)
			if !reIdent.MatchString(c) {
				return nil, false
			}
			cols = append(cols, unq(c))
		}
		return cols, len(cols) > 0
	}
	for _, m := range reCreateTable.FindAllStringSubmatchIndex(src, -1) {
		table := unq(src[m[2]:m[3]])
		depth, start := 1, m[1]
		end := start
		for end < len(src) && depth > 0 {
			switch src[end] {
			case '(':
				depth++
			case ')':
				depth--
			}
			end++
		}
		body := src[start : end-1]
		// 顶层的逗号切开(CHECK (...) 里的逗号不算)
		var items []string
		d, from := 0, 0
		for i, r := range body {
			switch r {
			case '(':
				d++
			case ')':
				d--
			case ',':
				if d == 0 {
					items = append(items, body[from:i])
					from = i + 1
				}
			}
		}
		items = append(items, body[from:])
		for _, it := range items {
			it = strings.TrimSpace(it)
			if km := reTableKey.FindStringSubmatch(it); km != nil {
				if cols, ok := list(km[2]); ok {
					out[table] = append(out[table], cols)
				}
				continue
			}
			// 列的定义: 括号里的东西(CHECK、DEFAULT 的表达式)去掉之后找 PRIMARY KEY / UNIQUE
			flat, d := "", 0
			for _, r := range it {
				switch {
				case r == '(':
					d++
				case r == ')':
					d--
				case d == 0:
					flat += string(r)
				}
			}
			f := strings.Fields(flat)
			if len(f) < 2 || strings.EqualFold(f[0], "CONSTRAINT") || strings.EqualFold(f[0], "CHECK") || strings.EqualFold(f[0], "FOREIGN") || strings.EqualFold(f[0], "EXCLUDE") {
				continue
			}
			if reColKey.MatchString(strings.Join(f[1:], " ")) {
				out[table] = append(out[table], []string{unq(f[0])})
			}
		}
	}
	for _, m := range reUniqueIndex.FindAllStringSubmatch(src, -1) {
		if strings.TrimSpace(m[3]) != "" { // WHERE(部分索引)/ INCLUDE 之类: 保守地不算
			if !regexp.MustCompile(`(?i)^\s*(INCLUDE\s*\([^()]*\))?\s*$`).MatchString(m[3]) {
				continue
			}
		}
		if cols, ok := list(m[2]); ok {
			out[unq(m[1])] = append(out[unq(m[1])], cols)
		}
	}
}

// relatedSource: 关联 / 子表是来源单元展开的逻辑表(领域的 存储: JSON版本 …)时: 对方 = 来源的子流水线
// (和数据来源是逻辑表时的流水线开头一样),唯一键 = 自然键(取最新的键;可以为空的键列里空值自己是一组)。
// lib = nil: 只要唯一键(设计编译器检查目录用)。普通的表: 什么都不做。
func relatedSource(s *spec.Spec, st *core.Step, cat *catalog, lib *cells.Library) error {
	if s.Domain == nil {
		return nil
	}
	e := s.Domain.Entities[st.Table]
	if e.Storage == nil {
		return nil
	}
	st.Unique = [][]string{e.NaturalKey}
	if lib == nil {
		return nil
	}
	src, ok := sources.ByKind(e.Storage.Kind)
	if !ok {
		return fmt.Errorf("表 %s: 存储 %s 不存在", st.Table, e.Storage.Kind)
	}
	scan, err := scanStep(e.Storage.Table, cat)
	if err != nil {
		return err
	}
	steps, err := src.Steps(e.Storage, e.NaturalKey, scan, lib)
	if err != nil {
		return fmt.Errorf("表 %s 的来源: %w", st.Table, err)
	}
	st.Source = steps
	return nil
}

// isLiteral: 条件的右边是常量('文本' / 数字): "对方的列 = 常量"。
func isLiteral(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '\'' {
		return true
	}
	if s[0] == '-' {
		s = s[1:]
	}
	return s != "" && s[0] >= '0' && s[0] <= '9'
}
