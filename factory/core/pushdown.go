package core

// 筛选下推(只用于生成 SQL;参考求值器跑原始流水线,差分测试因此也在验证这个改写)。
//
// 等式: filter(P) ∘ pick_per_key(K) = pick_per_key(K) ∘ filter(P),当 P 只引用 K 里的列。
// 理由: P 只看分组键,同一组的行对 P 的结果完全相同 —— 要么整组留下,要么整组去掉;
// 留下的组里取哪一条不变。
//
// 用途: 取最新版本(JSON版本来源)之后的"商品代码属于[...]"之类的条件,下推后数据库可以用索引直接定位,
// 而不是先给全部的键取最新版本再筛选。
//
// 只跨过 filter(筛选之间可以交换)和只做列改名的 project;遇到 sort / limit / 计算列就停。

// pushdown 返回改写后的流水线(不修改原来的)。没有可以下推的条件时返回原来的。
func pushdown(p *Pipeline) *Pipeline {
	steps := append([]Step(nil), p.Steps...)
	changed := false
	for k := 0; k < len(steps); k++ {
		if steps[k].Kind != "pick_per_key" {
			continue
		}
		keys := map[string]bool{}
		for _, x := range steps[k].Keys {
			keys[x] = true
		}
		// name: 当前位置的列名 → pick_per_key 输出里的列名(只跟踪简单改名)
		name := map[string]string{}
		for x := range keys {
			name[x] = x
		}
		var moved []*Expr
		for j := k + 1; j < len(steps); j++ {
			st := steps[j]
			if st.Kind == "project" {
				next := map[string]string{}
				for _, x := range st.Project {
					if x.Expr.Op == "col" {
						if src, ok := name[x.Expr.Name]; ok {
							next[x.Name] = src
						}
					}
				}
				name = next
				continue
			}
			if st.Kind != "filter" {
				break
			}
			var stay []*Expr
			for _, c := range conjuncts(st.Pred) {
				if r, ok := renameTo(c, name); ok {
					moved = append(moved, r)
				} else {
					stay = append(stay, c)
				}
			}
			if len(stay) == len(conjuncts(st.Pred)) {
				continue
			}
			switch len(stay) {
			case 0:
				steps = append(steps[:j], steps[j+1:]...)
				j--
			case 1:
				steps[j].Pred = stay[0]
			default:
				steps[j].Pred = And(stay...)
			}
		}
		if len(moved) == 0 {
			continue
		}
		changed = true
		pred := moved[0]
		if len(moved) > 1 {
			pred = And(moved...)
		}
		f := Step{Kind: "filter", Pred: pred, From: "下推(只引用分组键)"}
		steps = append(steps[:k], append([]Step{f}, steps[k:]...)...)
		k++
	}
	if !changed {
		return p
	}
	q := *p
	q.Steps = steps
	return &q
}

func conjuncts(e *Expr) []*Expr {
	if e.Op == "and" {
		var out []*Expr
		for _, a := range e.Args {
			out = append(out, conjuncts(a)...)
		}
		return out
	}
	return []*Expr{e}
}

// renameTo: 表达式里的列全部能映射到分组键时,返回改名后的副本。
// 有 tuple_after(游标,和排序绑定)或者没有引用任何列,就不动。
func renameTo(e *Expr, name map[string]string) (*Expr, bool) {
	refs := 0
	var walk func(e *Expr) (*Expr, bool)
	walk = func(e *Expr) (*Expr, bool) {
		if e == nil {
			return nil, true
		}
		switch e.Op {
		case "tuple_after":
			return nil, false
		case "col":
			n, ok := name[e.Name]
			if !ok {
				return nil, false
			}
			refs++
			return Col(n), true
		}
		c := *e
		c.Args = nil
		for _, a := range e.Args {
			r, ok := walk(a)
			if !ok {
				return nil, false
			}
			c.Args = append(c.Args, r)
		}
		return &c, true
	}
	r, ok := walk(e)
	return r, ok && refs > 0
}
