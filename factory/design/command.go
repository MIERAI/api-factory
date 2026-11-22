package design

// 命令(命令工厂,R3-1)的设计稿: 输入的 写入 / 键 / 乐观锁、种类的 设置、例子的 错误 / 之后。
//
//	输入:
//	  sku:     { 类型: 文本,   必填: 是, 键: sku }
//	  price:   { 类型: 十进制, 必填: 是, 写入: price }
//	  version: { 类型: 整数32, 必填: 是, 乐观锁: version }
//	设置: { updated_at: 现在 }
//	例子:
//	  用例:
//	    - 说明: …
//	      输入: { … }
//	      结果: { … }          # 或者 错误: 版本冲突 / 找不到 / 已存在 / 参数错误 / 前提不满足(或错误码)
//	      之后: 不变           # 或者 { 表: [ 全部的行(按主键对应,只比较写出来的列) ] }

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/exits"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

// commandBindings: 输入 → 命令的 键 / 写入 / 乐观锁;设置 的值按列的类型解释。查询的种类不能写这些键。
func (c *ctx) commandBindings(a *API, table string, e *errs) {
	if !a.Exit.Command {
		for _, in := range a.Inputs {
			if in.Write != "" || in.Key != "" || in.Lock != "" {
				e.add(in.Line, "输入 %s: 写入 / 键 / 乐观锁 只能用于命令(新建 / 按键更新 / 按键删除);查询的输入用 筛选", in.Name)
			}
		}
		return
	}
	cmd := a.Pattern.Command
	if cmd == nil {
		cmd = &spec.Command{}
		a.Pattern.Command = cmd
	}
	if len(a.Where) > 0 || len(a.Lookups)+len(a.Nests)+len(a.ChildAggs) > 0 || len(a.Derived) > 0 {
		e.add(1, "%s: 命令只写一张表,不能写 固定条件 / 关联 / 嵌套 / 子聚合 / 派生参数", a.Exit.Name)
	}
	if table == "" {
		e.add(1, "%s: 缺少 数据来源(写入的表)", a.Exit.Name)
		return
	}
	t := c.d.Tables[table]
	var pk []string
	if t != nil {
		pk = t.PrimaryKey
		if t.Storage != nil {
			e.add(1, "%s: 表 %s 是 存储: JSON版本 的逻辑表,不能直接写(写原始表)", a.Exit.Name, table)
		}
		if t.Policy == "versioned" {
			e.add(1, "%s: 表 %s 的更新方式是 只追加,不能按键更新 / 删除(新的版本用 新建 追加)", a.Exit.Name, table)
		}
	}
	kind := a.Kind
	column := func(in *Input, col, what string) bool {
		if _, ok := c.column(table, col); !ok {
			e.add(in.Line, "输入 %s: %s %s 不是表 %s 的列", in.Name, what, col, table)
			return false
		}
		return true
	}
	for _, in := range a.Inputs {
		n := 0
		for _, x := range []string{in.Write, in.Key, in.Lock} {
			if x != "" {
				n++
			}
		}
		switch {
		case in.Filter != "":
			e.add(in.Line, "输入 %s: 命令的输入不能写 筛选(用 写入 / 键 / 乐观锁)", in.Name)
			continue
		case n != 1:
			e.add(in.Line, "输入 %s: 命令的每个输入要写 写入 / 键 / 乐观锁 中的一个", in.Name)
			continue
		case strings.HasSuffix(in.Type, "列表") || in.Type == GroupListType:
			e.add(in.Line, "输入 %s: 命令的输入不能是列表(批量写在 R6)", in.Name)
			continue
		}
		switch {
		case in.Key != "":
			if kind == "insert" {
				e.add(in.Line, "输入 %s: 新建 的主键用 写入(键 只用于 按键更新 / 按键删除)", in.Name)
				continue
			}
			if !in.Required {
				e.add(in.Line, "输入 %s: 键 必须是 必填: 是", in.Name)
			}
			if column(in, in.Key, "键") {
				cmd.Key = append(cmd.Key, spec.Binding{Field: in.Name, Column: in.Key})
			}
		case in.Lock != "":
			if kind == "insert" {
				e.add(in.Line, "输入 %s: 新建 没有乐观锁(版本列的初始值用数据库的默认值)", in.Name)
				continue
			}
			if !in.Required {
				e.add(in.Line, "输入 %s: 乐观锁 必须是 必填: 是(读到的版本号)", in.Name)
			}
			if in.Type != "整数32" && in.Type != "整数64" {
				e.add(in.Line, "输入 %s: 乐观锁 的类型是 整数32 / 整数64", in.Name)
			}
			if cmd.Lock != nil {
				e.add(in.Line, "输入 %s: 乐观锁 只能有一个", in.Name)
				continue
			}
			if column(in, in.Lock, "乐观锁") {
				if col, _ := c.column(table, in.Lock); col.Type != "int4" && col.Type != "int8" {
					e.add(in.Line, "输入 %s: 乐观锁 的列 %s 必须是整数(现在是 %s)", in.Name, in.Lock, col.Type)
				}
				cmd.Lock = &spec.Binding{Field: in.Name, Column: in.Lock}
			}
		default:
			if kind == "delete" {
				e.add(in.Line, "输入 %s: 按键删除 没有 写入", in.Name)
				continue
			}
			if kind == "update" && !in.Required { // R3-1: 不做"不传 = 不改"(部分更新)
				e.add(in.Line, "输入 %s: 按键更新 的 写入 必须是 必填: 是(不传 = 不改 的部分更新还不支持)", in.Name)
			}
			if column(in, in.Write, "写入") {
				cmd.Write = append(cmd.Write, spec.Binding{Field: in.Name, Column: in.Write})
			}
		}
	}
	if kind != "insert" && t != nil && len(pk) > 0 {
		got := map[string]bool{}
		for _, k := range cmd.Key {
			got[k.Column] = true
		}
		ok := len(got) == len(pk)
		for _, p := range pk {
			ok = ok && got[p]
		}
		if !ok {
			e.add(1, "%s: 键 必须正好是表 %s 的主键 %v(按键 = 最多一行)", a.Exit.Name, table, pk)
		}
		for _, w := range cmd.Write {
			for _, p := range pk {
				if w.Column == p {
					e.add(1, "%s: 不能 写入 主键的列 %s", a.Exit.Name, p)
				}
			}
		}
	}
	if cmd.Lock != nil {
		for _, w := range cmd.Write {
			if w.Column == cmd.Lock.Column {
				e.add(1, "%s: 版本列 %s 由工厂加 1,不能 写入", a.Exit.Name, w.Column)
			}
		}
	}
	for i, s := range cmd.Set {
		v, err := c.setValue(table, s.Column, s.Value)
		if err != nil {
			e.add(1, "设置 %s: %v", s.Column, err)
			continue
		}
		cmd.Set[i].Value = v
		if kind == "delete" {
			e.add(1, "按键删除 没有 设置")
		}
		if cmd.Lock != nil && s.Column == cmd.Lock.Column {
			e.add(1, "设置 %s: 版本列由工厂加 1,不能设置", s.Column)
		}
	}
}

// setValue: 设置 的值 → spec 的写法。现在 → now()(列必须是时间);枚举列 → 数据库值;其他按列的类型检查。
func (c *ctx) setValue(table, col, v string) (string, error) {
	cc, ok := c.column(table, col)
	if !ok {
		return "", fmt.Errorf("不是表 %s 的列", table)
	}
	if v == "现在" {
		if cc.Type != "timestamptz" {
			return "", fmt.Errorf("现在 只能设置到时间的列(%s 是 %s)", col, cc.Type)
		}
		return spec.NowValue, nil
	}
	if t := c.d.Tables[table]; t != nil && t.ColEnum[col] != "" {
		en := c.d.Enum(t.ColEnum[col])
		for _, x := range en.Values {
			if x.Name == v {
				return strconv.FormatInt(x.DB, 10), nil
			}
		}
		var names []string
		for _, x := range en.Values {
			names = append(names, x.Name)
		}
		return "", fmt.Errorf("%q 不是枚举 %s 的值(可以用: %v)", v, en.Name, names)
	}
	k, ok := kinds.BySQL(cc.Type)
	if !ok {
		return "", fmt.Errorf("列的类型 %s 还不支持", cc.Type)
	}
	switch k.Value {
	case "int":
		if _, err := strconv.ParseInt(v, 10, 64); err != nil {
			return "", fmt.Errorf("%q 不是整数", v)
		}
	case "bool":
		switch v {
		case "是", "true":
			return "true", nil
		case "否", "false":
			return "false", nil
		}
		return "", fmt.Errorf("%q: 布尔写 是 / 否", v)
	case "time":
		return "", fmt.Errorf("时间的列只能设置 现在")
	}
	if k.Parse != nil {
		if _, err := k.Parse(v); err != nil {
			return "", err
		}
	}
	return v, nil
}

// commandCase: 命令的一个用例: 结果 或 错误(二选一)+ 之后(必填)。
func (c *ctx) commandCase(a *API, resp *message, n *yaml.Node) (spec.Expect, *spec.After, error) {
	res, er := get(n, "结果"), get(n, "错误")
	var ex spec.Expect
	switch {
	case res != nil && er != nil:
		return ex, nil, fmt.Errorf("第 %d 行: 结果 和 错误 只能写一个", n.Line)
	case er != nil:
		w := strings.TrimSpace(er.Value)
		code, ok := exits.CommandErrors[w]
		if !ok && w == strings.ToUpper(w) && w != "" && w != "OK" {
			code, ok = w, true
		}
		if !ok {
			return ex, nil, fmt.Errorf("第 %d 行: 错误 %q 无法识别(找不到 / 版本冲突 / 已存在 / 参数错误 / 前提不满足,或者错误码)", er.Line, w)
		}
		ex = spec.Expect{Code: code}
	case res != nil:
		var err error
		if ex, err = c.expect(resp, "", res); err != nil {
			return ex, nil, err
		}
		if ex.Code != "OK" {
			return ex, nil, fmt.Errorf("第 %d 行: 命令的失败写在 错误: 里(结果 只写成功时的输出)", res.Line)
		}
	default:
		return ex, nil, fmt.Errorf("第 %d 行: 命令的用例要写 结果(成功时的输出)或 错误", n.Line)
	}
	af := get(n, "之后")
	if af == nil {
		return ex, nil, fmt.Errorf("第 %d 行: 命令的用例要写 之后(请求之后的数据;没有变化写 不变)", n.Line)
	}
	if af.Kind == yaml.ScalarNode {
		if strings.TrimSpace(af.Value) != "不变" {
			return ex, nil, fmt.Errorf("第 %d 行: 之后 写 不变,或者 { 表: [ 之后的全部行 ] }", af.Line)
		}
		return ex, &spec.After{Unchanged: true}, nil
	}
	after := &spec.After{Tables: map[string][]map[string]any{}}
	for _, p := range pairs(af) {
		if _, ok := c.cat[p.Key]; !ok {
			return ex, nil, fmt.Errorf("第 %d 行: 之后: %s 不是数据库里的表", p.Line, p.Key)
		}
		if p.Val.Kind != yaml.SequenceNode {
			return ex, nil, fmt.Errorf("第 %d 行: 之后.%s 写成行的列表(之后的全部行;空表写 [])", p.Line, p.Key)
		}
		rows, err := c.fixtureRows(p.Key, p.Val.Content)
		if err != nil {
			return ex, nil, fmt.Errorf("之后 %s: %w", p.Key, err)
		}
		if t := c.d.Tables[p.Key]; t != nil {
			for i, r := range rows {
				for _, k := range t.PrimaryKey {
					if _, ok := r[k]; !ok {
						return ex, nil, fmt.Errorf("之后 %s 的第 %d 行: 要写主键 %s(按主键和数据库的行对应)", p.Key, i+1, k)
					}
				}
			}
		}
		after.Tables[p.Key] = rows
	}
	return ex, after, nil
}
