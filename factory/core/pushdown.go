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
// 只跨过 filter(筛选之间可以交换)、只做列改名的 project 和 lookup;遇到 sort / limit / 计算列 / 其他元件就停。
//
// 越过 lookup(多对一查找)的等式: filter(P) ∘ lookup(L) = lookup(L) ∘ filter(P),当 P 不引用 L 加的列。
// 理由: lookup 对每一行单独判断(最多匹配一行): keep 原样留下每一行、只加列;drop 只去掉找不到的行。
// 两种都不改已有的列、不改变行之间的关系 —— 只看已有的列的条件 P 在 lookup 之前和之后对每一行的结果相同。
// 引用加的列的条件(包括 is_null(加的列)、和已有列 or 在一起的条件)一律不下推: 找不到时(keep)加的列是 NULL,
// 同名的已有列不存在(检查禁止重名)但为了保险,加的列名从改名表里去掉。
// 用途: 关联之后的"商品代码属于[...]"(catalog.list-labels-with-product)下推到来源的取最新之前,用得上 (键, 登记顺序) 的索引。

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
			if st.Kind == "lookup" { // 见文件开头: 只加列 / 逐行去掉找不到的行,已有的列不变
				for _, t := range st.Take {
					delete(name, t.Name)
				}
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
// 没有引用任何列就不动。游标(tuple_after)也一样: 列全是分组键时,同一组的行对它的结果相同,可以下推
// (每组取一条 + 分页: 每一页只给游标之后的键取最新,不用每页都给全部的键取一遍)。
func renameTo(e *Expr, name map[string]string) (*Expr, bool) {
	refs := 0
	var walk func(e *Expr) (*Expr, bool)
	walk = func(e *Expr) (*Expr, bool) {
		if e == nil {
			return nil, true
		}
		switch e.Op {
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
