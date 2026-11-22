package core

// lookup 的两个扩展(prim_lookup.go 的 check / eval / SQL 调用这里):
//
// 1. Pred: 对方的列上的条件(设计稿的 `对方的列 = 常量`)。先筛对方的行(UNKNOWN 丢弃),再按键匹配。
//    唯一性: Pred 里顶层的 `列 = 常量`(and 的各项)固定了那些列 → 和关联的键一起覆盖唯一键也算最多一行。
// 2. AsOf: 按日期查历史表。对方的行: 键相等 + Pred + 开始 ≤ 参照 + (有结束列时)结束为空或 参照 < 结束,
//    其中开始最大的一行(唯一键 = 键 + 开始 → 最多一行)。EndIn: 结束列是最后一天(含): 参照 <= 结束。参照是当前行上的表达式(列 / 今天 …);参照或开始为空 → 不匹配。
//    找不到时和普通的 lookup 一样(keep: 加的列为空;drop: 丢弃这一行)。
//    SQL: LEFT JOIN LATERAL (SELECT 加的列 FROM 对方 WHERE 键 AND 开始 <= 参照 AND (结束 IS NULL OR 参照 < 结束) AND Pred
//         ORDER BY 开始 DESC LIMIT 1) ON TRUE(drop: JOIN LATERAL)。
//    差分测试的"错误实现"(AsOf.Mutant,参考求值器认,SQL 后端和 IR 里没有): start_lt(开始 < 参照)/ end_flip(含 ↔ 不含)/
//    no_end(不看结束)/ min(取开始最小的)。

import (
	"fmt"
	"strings"
)

// AsOf: 按日期查历史表的参数。
type AsOf struct {
	Start  string `json:"start"`                   // 对方的开始列(含)
	End    string `json:"end,omitempty"`           // 对方的结束列(不含;空值 = 无期限)。空 = 没有结束列
	EndIn  bool   `json:"end_inclusive,omitempty"` // 结束列是最后一天(含): 参照 <= 结束
	Ref    *Expr  `json:"ref"`                     // 参照: 当前行上的表达式
	Mutant string `json:"-"`
}

// PinnedCols: Pred 里顶层的 `列 = 常量` 固定了的列。
func PinnedCols(pred *Expr) map[string]bool {
	out := map[string]bool{}
	var walk func(e *Expr)
	walk = func(e *Expr) {
		if e == nil {
			return
		}
		switch {
		case e.Op == "and":
			for _, a := range e.Args {
				walk(a)
			}
		case e.Op == "cmp" && e.Name == "=" && len(e.Args) == 2:
			a, b := e.Args[0], e.Args[1]
			if a.Op == "const" && b.Op == "col" {
				a, b = b, a
			}
			if a.Op == "col" && b.Op == "const" && b.Value != nil {
				out[a.Name] = true
			}
		}
	}
	walk(pred)
	return out
}

// checkLookupExt: Pred / AsOf 的 DRC;返回唯一性用的另外覆盖的列(固定的列 + 开始列)。
func (p *Pipeline) checkLookupExt(cur Schema, st Step, at string, add func(string, ...any)) map[string]bool {
	other := Schema(st.Columns)
	extra := map[string]bool{}
	if st.Pred != nil {
		t, err := p.typeOf(st.Pred, other)
		switch {
		case err != nil:
			add("%s: 对方的条件: %v", at, err)
		case t.Kind != Bool:
			add("%s: 对方的条件必须是 bool,实际是 %s", at, t.Kind)
		}
		if refersRequest(st.Pred) {
			add("%s: 对方的条件不能引用请求参数", at)
		}
		for c := range PinnedCols(st.Pred) {
			extra[c] = true
		}
	}
	if a := st.AsOf; a != nil {
		if len(st.Source) > 0 {
			add("%s: 按日期的关联还不支持逻辑表", at)
		}
		s, ok := other.Col(a.Start)
		if !ok {
			add("%s: %s 没有开始列 %s", at, st.Table, a.Start)
		}
		if a.Ref == nil {
			add("%s: 按日期的关联需要参照", at)
			return extra
		}
		rt, err := p.typeOf(a.Ref, cur)
		if err != nil {
			add("%s: 参照: %v", at, err)
			return extra
		}
		if ok && (!comparable(s.Kind, rt.Kind) || (s.Kind != Date && s.Kind != Timestamp && s.Kind != Int32 && s.Kind != Int64)) {
			add("%s: 开始列 %s(%s)和参照(%s)不能比较(要都是日期 / 都是时间)", at, a.Start, s.Kind, rt.Kind)
		}
		if a.End != "" {
			e, ok := other.Col(a.End)
			switch {
			case !ok:
				add("%s: %s 没有结束列 %s", at, st.Table, a.End)
			case e.Kind != s.Kind:
				add("%s: 结束列 %s(%s)和开始列 %s(%s)的类型不同", at, a.End, e.Kind, a.Start, s.Kind)
			}
		}
		extra[a.Start] = true
	} else if len(st.Source) > 0 && st.Pred != nil {
		add("%s: 对方的条件还不支持逻辑表", at)
	}
	return extra
}

// asOfPick: 匹配的行(键相等、过了 Pred)里按日期选一行(没有 → nil)。
func (p *Pipeline) asOfPick(st Step, r Row, schema Schema, params Params, found []Row) (Row, error) {
	a := st.AsOf
	ref, err := p.eval(a.Ref, r, schema, params)
	if err != nil || ref == nil {
		return nil, err
	}
	var best Row
	for _, o := range found {
		s := o[a.Start]
		if s == nil {
			continue
		}
		c := compareValues(s, ref)
		if c > 0 || (c == 0 && a.Mutant == "start_lt") {
			continue
		}
		if a.End != "" && a.Mutant != "no_end" {
			if e := o[a.End]; e != nil {
				ce := compareValues(ref, e)
				inclusive := a.EndIn != (a.Mutant == "end_flip")
				if ce > 0 || (ce == 0 && !inclusive) {
					continue
				}
			}
		}
		if best == nil {
			best = o
			continue
		}
		cb := compareValues(s, best[a.Start])
		if (a.Mutant == "min" && cb < 0) || (a.Mutant != "min" && cb > 0) {
			best = o
		}
	}
	return best, nil
}

// lookupLateralSQL: 按日期的关联(或带对方的条件的关联)的 LATERAL 子查询。
func (g *sqlGen) lookupLateralSQL(lv *sqlLevel, st Step) (*sqlLevel, error) {
	g.n++
	inner := &sqlLevel{alias: fmt.Sprintf("t%d", g.n), in: Schema(st.Columns), out: Schema(st.Columns)}
	var where []string
	for _, k := range st.On {
		where = append(where, fmt.Sprintf("%s = %s", g.col(inner, k.Key), g.col(lv, k.Col)))
	}
	if st.Pred != nil {
		w, err := g.where(st.Pred, inner)
		if err != nil {
			return nil, err
		}
		where = append(where, w)
	}
	order := ""
	if a := st.AsOf; a != nil {
		ref, err := g.expr(a.Ref, lv)
		if err != nil {
			return nil, err
		}
		where = append(where, fmt.Sprintf("%s <= %s", g.col(inner, a.Start), ref))
		if a.End != "" {
			end := g.col(inner, a.End)
			op := "<"
			if a.EndIn {
				op = "<="
			}
			where = append(where, fmt.Sprintf("(%s IS NULL OR %s %s %s)", end, ref, op, end))
		}
		order = fmt.Sprintf("\nORDER BY %s DESC\nLIMIT 1", g.col(inner, a.Start))
	}
	var cols []string
	seen := map[string]bool{}
	for _, t := range st.Take {
		if !seen[t.Col] {
			seen[t.Col] = true
			cols = append(cols, g.col(inner, t.Col))
		}
	}
	sub := fmt.Sprintf("SELECT %s\nFROM %s AS %s\nWHERE %s%s", strings.Join(cols, ", "), quoteIdent(st.Table), inner.alias, strings.Join(where, "\n  AND "), order)
	g.n++
	alias := fmt.Sprintf("t%d", g.n)
	join := "LEFT JOIN LATERAL"
	if st.Missing == "drop" {
		join = "JOIN LATERAL"
	}
	lv.from += fmt.Sprintf("\n%s (\n%s\n) AS %s ON TRUE", join, indent(sub), alias)
	if lv.refs == nil {
		lv.refs = map[string]string{}
	}
	for _, t := range st.Take {
		lv.refs[t.Name] = alias + "." + quoteIdent(t.Col)
	}
	return lv, nil
}
