package core

// 索引的键的条件(只用于生成 SQL;参考求值器跑原始流水线,差分测试因此也在验证这个改写)。
//
// JSON 字段(json_field)的整数 / 十进制 / 真假 / 日期的值在 SQL 里是带校验的转换(不合法 → 查询失败),这个式子不能放进
// 表达式索引(脏数据会写不进去)。所以 JSON 字段上的比较条件另外生成一份用 json_key(不报错、可以放进索引的式子;
// expr_json_key.go)写的条件,放在展开 JSON 的 project 之前(表的那一层,数据库可以直接用索引):
//
//	project(c := json_field(x, k, T)) → filter(C(c))
//	  ⇒ filter(C(json_key(x, k, T))) → project(c := json_field(x, k, T)) → filter(C(c))
//
// 原来的条件留着(读到的行照样校验)。加的条件是原来的条件推得出来的: 合法的值上 json_key = json_field,
// 所以两个条件对这一行的结果相同;json_field 为 NULL(键不存在 / JSON null)时 json_key 也是 NULL。
// 不同只在不合法的值上: 原来的条件让查询失败,加的条件让这一行在读之前就被排除(和按键用索引读的时候一样:
// 只校验读到的行)。
//
// 只认这些形状(一个 JSON 字段,能用索引的比较): cmp(= < <= > >=, c, R) / cmp(op, R, c) / in_list(c, 列表参数),
// 以及 or(absent(参数), 上面的形状)(不传 = 不筛选的写法);R 不引用列(参数 / 常量 / 外层的列 sql_ref)。
// 文本的 json_field 本来就和 json_key 是同一个式子,不用改写。时刻不能做索引的键(JSONKeySQL),不改写。

// keyIndex 返回改写后的流水线(不修改原来的)。没有可以改写的条件时返回原来的。
func keyIndex(p *Pipeline) *Pipeline {
	var out []Step
	changed := false
	for i, st := range p.Steps {
		if st.Kind != "project" {
			out = append(out, st)
			continue
		}
		defs := map[string]*Expr{} // 列 → 它的 json_key(只收能改写的 JSON 字段)
		for _, x := range st.Project {
			e := x.Expr
			if e.Op != "json_field" || e.Type == nil || e.Type.Kind == Text || len(e.Args) != 1 || e.Args[0].Op != "col" {
				continue
			}
			if _, ok := JSONKeySQL("v", e.Type.Kind); ok {
				defs[x.Name] = JSONKey(e.Args[0], e.Name, *e.Type)
			}
		}
		var add []*Expr
		for j := i + 1; len(defs) > 0 && j < len(p.Steps) && p.Steps[j].Kind == "filter"; j++ {
			for _, c := range conjuncts(p.Steps[j].Pred) {
				if r, ok := keyCond(c, defs); ok {
					add = append(add, r)
				}
			}
		}
		if len(add) > 0 {
			changed = true
			pred := add[0]
			if len(add) > 1 {
				pred = And(add...)
			}
			out = append(out, Step{Kind: "filter", Pred: pred, From: "索引的键(json_key)"})
		}
		out = append(out, st)
	}
	if !changed {
		return p
	}
	q := *p
	q.Steps = out
	return &q
}

var keyCmpOps = map[string]bool{"=": true, "<": true, "<=": true, ">": true, ">=": true}

// keyCond: 条件 c 是能改写的形状时,返回把 JSON 字段的列换成 json_key 的条件。
func keyCond(c *Expr, defs map[string]*Expr) (*Expr, bool) {
	if c.Op == "or" && len(c.Args) == 2 && c.Args[0].Op == "absent" {
		r, ok := keyCond(c.Args[1], defs)
		if !ok {
			return nil, false
		}
		return Or(c.Args[0], r), true
	}
	key := func(x *Expr) *Expr {
		if x.Op == "col" {
			return defs[x.Name]
		}
		return nil
	}
	free := func(x *Expr) bool { return x.Op == "param" || x.Op == "const" || x.Op == "sql_ref" }
	switch {
	case c.Op == "cmp" && keyCmpOps[c.Name] && len(c.Args) == 2:
		if k := key(c.Args[0]); k != nil && free(c.Args[1]) {
			return Cmp(c.Name, k, c.Args[1]), true
		}
		if k := key(c.Args[1]); k != nil && free(c.Args[0]) {
			return Cmp(c.Name, c.Args[0], k), true
		}
	case c.Op == "in_list" && len(c.Args) == 1:
		if k := key(c.Args[0]); k != nil {
			return InList(k, c.Name), true
		}
	}
	return nil, false
}
