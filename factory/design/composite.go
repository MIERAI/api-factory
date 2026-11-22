package design

// 组合命令(命令工厂 R3-2)的设计稿: 操作的列表(新建 / 幂等新建 / 更新 / 删除,一个事务)、条件更新的 条件 / 不满足、
// 幂等新建的 键、后面的操作引用前面操作的返回值(操作名.列)、输出的 来自(操作名.列 / 操作名.新建)。
// 这里检查表 / 列 / 主键 / 唯一键 / 类型,把表达式规范化(枚举的值名 → 数据库值,现在 → now())写进 spec。

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"apifactory/factory/command"
	"apifactory/factory/compiler"
	"apifactory/factory/exits"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

var placeholderInMsg = regexp.MustCompile(`\{([^{}]+)\}`)

// compositeOps: 组合命令的输入和操作。
func (c *ctx) compositeOps(a *API, e *errs) {
	cmd := a.Pattern.Command
	if a.Source != "" {
		e.add(1, "组合命令: 不写 数据来源(每个操作写自己的表)")
	}
	if len(a.Where) > 0 || len(a.Lookups)+len(a.Nests)+len(a.ChildAggs) > 0 || len(a.Derived) > 0 {
		e.add(1, "组合命令: 不能写 固定条件 / 关联 / 嵌套 / 子聚合 / 派生参数")
	}
	inputs := map[string]*Input{}
	for _, in := range a.Inputs {
		inputs[in.Name] = in
		switch {
		case in.Write != "" || in.Key != "" || in.Lock != "" || in.Filter != "":
			e.add(in.Line, "输入 %s: 组合命令的输入不写 写入 / 键 / 乐观锁 / 筛选(操作里用 输入.%s 引用)", in.Name, in.Name)
		case strings.HasSuffix(in.Type, "列表") || in.Type == GroupListType:
			e.add(in.Line, "输入 %s: 组合命令的输入不能是列表(对列表逐项执行的 每项 在 R4)", in.Name)
		}
	}
	optionalOK := map[string]bool{} // 可以不传的输入: 只能直接作为 新建 / 幂等新建 的值(不传 = 不写这一列: 默认值 / 空)
	for name, in := range inputs {
		if in.Required {
			continue
		}
		ok := c.d.Enum(in.Type) == nil
		uses := 0
		for _, op := range cmd.Ops {
			all := append(append([]spec.ColExpr{}, op.Key...), op.Values...)
			if op.Lock != nil {
				all = append(all, *op.Lock)
			}
			for _, x := range all {
				if !strings.Contains(x.Expr, command.InputPrefix+name) {
					continue
				}
				uses++
				if strings.TrimSpace(x.Expr) != command.InputPrefix+name || (op.Kind != "insert" && op.Kind != "insert_idempotent") || containsCol(op.Key, x.Column) || contains(op.Conflict, x.Column) {
					ok = false
				}
			}
			if strings.Contains(op.Cond, command.InputPrefix+name) {
				ok = false
			}
		}
		optionalOK[name] = ok && uses > 0
		if !optionalOK[name] {
			e.add(in.Line, "输入 %s: 组合命令里可以不传的输入只能直接写进 新建 / 幂等新建 的值(不是键、不在条件和运算里、不是枚举);其他的用法要 必填: 是", name)
		}
	}
	used := map[string]bool{}
	inputType := func(name string) (string, error) {
		in := inputs[name]
		if in == nil {
			return "", fmt.Errorf("输入.%s: 没有这个输入", name)
		}
		used[name] = true
		if c.d.Enum(in.Type) != nil {
			return "enum", nil
		}
		k, ok := kinds.ByName(in.Type)
		if !ok {
			return "", fmt.Errorf("输入.%s: 类型 %q 不能用于写操作", name, in.Type)
		}
		for k.Base != "" && len(k.SQL) == 0 {
			b, ok := kinds.ByName(k.Base)
			if !ok {
				break
			}
			k = b
		}
		if len(k.SQL) == 0 {
			return "", fmt.Errorf("输入.%s: 类型 %q 不能用于写操作", name, in.Type)
		}
		return k.SQL[0], nil
	}
	uniques, err := compiler.UniqueKeys()
	if err != nil {
		e.add(1, "%v", err)
	}
	tables := map[string]string{} // 操作名 → 表
	for i := range cmd.Ops {
		op := &cmd.Ops[i]
		where := "操作 " + op.Name
		t := c.d.Tables[op.Table]
		if _, ok := c.cat[op.Table]; !ok || t == nil {
			e.add(1, "%s: 表 %s 不是这个领域的表(领域.yaml 的 表 + db/schema)", where, op.Table)
			continue
		}
		if t.Storage != nil {
			e.add(1, "%s: 表 %s 是 存储: JSON版本 的逻辑表,不能直接写", where, op.Table)
		}
		if t.Policy == "versioned" && (op.Kind == "update" || op.Kind == "delete") {
			e.add(1, "%s: 表 %s 的更新方式是 只追加,不能更新 / 删除", where, op.Table)
		}
		colType := func(col string) (string, bool) {
			cc, ok := c.column(op.Table, col)
			return cc.Type, ok
		}
		sc := command.Scope{
			Table: op.Table,
			Ref: func(ref, col string) (string, error) {
				rt, ok := tables[ref]
				if !ok {
					return "", fmt.Errorf("%s.%s: %s 不是前面的操作(只能引用写在前面的操作)", ref, col, ref)
				}
				if col == exits.CreatedWord {
					return "", fmt.Errorf("%s.新建 只能用于输出(仅当 在 R4)", ref)
				}
				cc, ok := c.column(rt, col)
				if !ok {
					return "", fmt.Errorf("%s.%s: 表 %s 没有列 %s", ref, col, rt, col)
				}
				return cc.Type, nil
			},
			Input: inputType,
			Enum: func(table, col, name string) (string, bool) {
				tt := c.d.Tables[table]
				if tt == nil || tt.ColEnum[col] == "" {
					return "", false
				}
				for _, v := range c.d.Enum(tt.ColEnum[col]).Values {
					if v.Name == name {
						return strconv.FormatInt(v.DB, 10), true
					}
				}
				return "", false
			},
			EnumOf: func(table, col string) bool {
				tt := c.d.Tables[table]
				return tt != nil && tt.ColEnum[col] != ""
			},
		}
		// 值: 列 ← 表达式(want = 列的类型;枚举的列 = enum:表.列)
		norm := func(what string, x *spec.ColExpr, rowCols bool) {
			cc, ok := c.column(op.Table, x.Column)
			if !ok {
				e.add(1, "%s: %s %s 不是表 %s 的列", where, what, x.Column, op.Table)
				return
			}
			tree, err := command.ParseExpr(x.Expr, false)
			if err != nil {
				e.add(1, "%s: %s %s: %v", where, what, x.Column, err)
				return
			}
			want, wt, wc := cc.Type, "", ""
			if sc.EnumOf(op.Table, x.Column) {
				want, wt, wc = "enum:"+op.Table+"."+x.Column, op.Table, x.Column
			}
			s2 := sc
			if rowCols {
				s2.Column = colType
			}
			if err := command.Resolve(tree, s2, want, wt, wc); err != nil {
				e.add(1, "%s: %s %s = %s: %v", where, what, x.Column, x.Expr, err)
				return
			}
			x.Expr = command.Format(tree)
		}
		seen := map[string]string{}
		mark := func(what, col string) {
			if prev, dup := seen[col]; dup {
				e.add(1, "%s: 列 %s 写了两次(%s、%s)", where, col, prev, what)
			}
			seen[col] = what
		}
		switch op.Kind {
		case "insert", "insert_idempotent":
			for j := range op.Values {
				mark("值", op.Values[j].Column)
				norm("值", &op.Values[j], false)
			}
			if op.Kind == "insert_idempotent" {
				if !isUniqueKey(uniques[op.Table], t.PrimaryKey, op.Conflict) {
					e.add(1, "%s: 幂等新建 的 键 %v 必须正好是表 %s 的主键或唯一约束(冲突只认这个键)", where, op.Conflict, op.Table)
				}
				for _, k := range op.Conflict {
					if _, ok := seen[k]; !ok {
						e.add(1, "%s: 幂等新建 的键 %s 要写在 值 里(按它找已有的行)", where, k)
					}
					if cc, ok := c.column(op.Table, k); ok && !cc.NotNull {
						e.add(1, "%s: 幂等新建 的键 %s 可能为空(空值不冲突,会重复插入): 列要 NOT NULL", where, k)
					}
				}
			}
		case "update", "delete":
			for j := range op.Key {
				mark("键", op.Key[j].Column)
				norm("键", &op.Key[j], false)
			}
			got := map[string]bool{}
			for _, k := range op.Key {
				got[k.Column] = true
			}
			okPK := len(got) == len(t.PrimaryKey)
			for _, p := range t.PrimaryKey {
				okPK = okPK && got[p]
			}
			if !okPK {
				e.add(1, "%s: 键 必须正好是表 %s 的主键 %v(按键 = 最多一行)", where, op.Table, t.PrimaryKey)
			}
			for j := range op.Values {
				mark("设置", op.Values[j].Column)
				norm("设置", &op.Values[j], true)
				if got[op.Values[j].Column] {
					e.add(1, "%s: 不能 设置 主键的列 %s", where, op.Values[j].Column)
				}
			}
			if op.Lock != nil {
				mark("乐观锁", op.Lock.Column)
				if cc, ok := c.column(op.Table, op.Lock.Column); !ok || (cc.Type != "int4" && cc.Type != "int8") {
					e.add(1, "%s: 乐观锁 的列 %s 必须是表的整数列", where, op.Lock.Column)
				}
				if !strings.HasPrefix(op.Lock.Expr, command.InputPrefix) {
					e.add(1, "%s: 乐观锁 的值写 输入.x(读到的版本号)", where)
				}
				norm("乐观锁", op.Lock, false)
			}
			if op.Cond != "" {
				tree, err := command.ParseExpr(op.Cond, true)
				if err == nil {
					s2 := sc
					s2.Column = colType
					err = command.Resolve(tree, s2, "", "", "")
				}
				if err != nil {
					e.add(1, "%s: 条件: %v", where, err)
				} else {
					op.Cond = command.Format(tree)
				}
			}
			for _, m := range placeholderInMsg.FindAllStringSubmatch(op.NotFound, -1) {
				if inputs[m[1]] == nil {
					e.add(1, "%s: 找不到时 的 {%s} 不是输入", where, m[1])
				}
			}
		}
		if op.Cond != "" && op.Kind != "update" {
			e.add(1, "%s: 条件 只能用于 更新", where)
		}
		if _, dup := tables[op.Name]; dup {
			e.add(1, "%s: 操作名重复", where)
		}
		tables[op.Name] = op.Table
	}
	for _, in := range a.Inputs {
		if !used[in.Name] && inputs[in.Name] != nil {
			e.add(in.Line, "输入 %s: 没有操作用到它", in.Name)
		}
	}
	// 输出: 来自 操作名.列 / 操作名.新建
	cmd.Output = nil
	for _, o := range a.Outputs {
		ref, col, ok := strings.Cut(o.From, ".")
		if !ok {
			e.add(o.Line, "输出 %s: 组合命令的输出要写 来自: 操作名.列(或 操作名.新建)", o.Name)
			continue
		}
		var kind string
		for _, op := range cmd.Ops {
			if op.Name == ref {
				kind = op.Kind
			}
		}
		switch {
		case kind == "":
			e.add(o.Line, "输出 %s: 来自 %s: 没有操作 %s", o.Name, o.From, ref)
			continue
		case col == exits.CreatedWord && kind != "insert_idempotent":
			e.add(o.Line, "输出 %s: 来自 %s: 新建 只有 幂等新建 有", o.Name, o.From)
			continue
		case col == exits.CreatedWord:
			cmd.Output = append(cmd.Output, spec.OutputFrom{Field: o.Name, Op: ref, Created: true})
			continue
		}
		if _, ok := c.column(tables[ref], col); !ok {
			e.add(o.Line, "输出 %s: 来自 %s: 表 %s 没有列 %s", o.Name, o.From, tables[ref], col)
			continue
		}
		cmd.Output = append(cmd.Output, spec.OutputFrom{Field: o.Name, Op: ref, Column: col})
	}
}

// compositeFields: 组合命令的响应字段(类型 = 来源的列;新建 = 布尔)。
func (c *ctx) compositeFields(a *API, e *errs) []*field {
	cmd := a.Pattern.Command
	tables := map[string]string{}
	for _, op := range cmd.Ops {
		tables[op.Name] = op.Table
	}
	byField := map[string]spec.OutputFrom{}
	for _, o := range cmd.Output {
		byField[o.Field] = o
	}
	var out []*field
	for _, o := range a.Outputs {
		src, ok := byField[o.Name]
		if !ok {
			continue // 已经报过错
		}
		if o.Type != "" {
			e.add(o.Line, "输出 %s: 组合命令的输出不写 类型(= 来源的列的类型)", o.Name)
		}
		f := &field{Name: o.Name, Desc: o.Desc}
		if src.Created {
			f.Type, f.kind = "bool", "bool"
			out = append(out, f)
			continue
		}
		pt, kind, opt, err := c.columnField(tables[src.Op], src.Column)
		if err != nil {
			e.add(o.Line, "输出 %s: %v", o.Name, err)
			continue
		}
		f.Type, f.kind, f.Optional = pt, kind, opt
		out = append(out, f)
	}
	return out
}

func containsCol(xs []spec.ColExpr, col string) bool {
	for _, x := range xs {
		if x.Column == col {
			return true
		}
	}
	return false
}

// isUniqueKey: cols 正好是主键或某个唯一键(顺序无关)。
func isUniqueKey(uniques [][]string, pk, cols []string) bool {
	same := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		m := map[string]bool{}
		for _, x := range a {
			m[x] = true
		}
		for _, x := range b {
			if !m[x] {
				return false
			}
		}
		return true
	}
	if same(pk, cols) {
		return true
	}
	for _, u := range uniques {
		if same(u, cols) {
			return true
		}
	}
	return false
}

// compositeTables: 组合命令写的表(准备数据里没写 = 空表;之后 / 状态比较的对象)。
func compositeTables(cmd *spec.Command) []string {
	var out []string
	seen := map[string]bool{}
	for _, op := range cmd.Ops {
		if !seen[op.Table] {
			seen[op.Table] = true
			out = append(out, op.Table)
		}
	}
	return out
}
