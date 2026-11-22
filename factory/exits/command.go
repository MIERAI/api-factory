package exits

// 命令工厂(写入,R3-1): 新建 / 按键更新 / 按键删除。
//
// 和查询的出口不同: 没有查询(Cell 为空、没有流水线),handler 把写操作作为数据交给运行时的事务执行器
// (api-factory-runtime/txn)。输入用 写入 / 键 / 乐观锁 对应到列(设计编译器解析,factory/design),
// 这里只登记种类、种类自己的键(设置 / 找不到时)和规格检查。
// 验收: 人写的例子(带 之后 的状态)+ 状态差分测试(factory/command: 内存里的模型状态 vs 数据库)。

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/ir"
	"apifactory/factory/spec"
)

// ⚖️ D1(命令工厂设计稿 §3.2,待负责人确认): 乐观锁的版本冲突 → ABORTED(gRPC / Connect 的惯例: 并发冲突,
// 调用方重读之后重试)。备选 FAILED_PRECONDITION。改这里: 设计稿的 错误: 版本冲突、生成代码的错误映射
// (adapters/golang/command.go)、模型状态的期待值(factory/command)都用这个常量。
const ConflictCode = "ABORTED"

// CommandErrors: 例子的 错误: 的写法 → 错误码(也可以直接写错误码)。
var CommandErrors = map[string]string{
	"找不到": "NOT_FOUND", "版本冲突": ConflictCode, "已存在": "ALREADY_EXISTS",
	"参数错误": "INVALID_ARGUMENT", "前提不满足": "FAILED_PRECONDITION",
}

// CommandKeys: 命令的种类在设计稿里另外可以写的键。
var CommandKeys = []string{"设置", "找不到时"}

func init() {
	for _, k := range []struct{ kind, name string }{{"insert", "新建"}, {"update", "按键更新"}, {"delete", "按键删除"}} {
		kind := k.kind
		keys := CommandKeys
		if kind == "insert" {
			keys = []string{"设置"}
		}
		if kind == "delete" {
			keys = []string{"找不到时"}
		}
		Register(&Exit{
			Kind: kind, Name: k.name, Cardinality: "one", Command: true, FromTable: true,
			DesignKeys: keys,
			ScalarKeys: []string{"找不到时"},
			Parse: func(d Design, p *spec.Pattern) {
				if p.Command == nil {
					p.Command = &spec.Command{}
				}
				if kind != "insert" {
					p.NotFoundMessage = d.Str("找不到时")
					if p.NotFoundMessage == "" {
						d.Err("%s: 缺少 找不到时(行不存在时返回的消息,例: \"product not found: {sku}\")", k.name)
					}
				}
				if n := d.Node("设置"); n != nil {
					if n.Kind != yaml.MappingNode {
						d.Err("设置: 写成 { 列: 值 }(值 = 现在 / 固定值 / 枚举的值名)")
						return
					}
					for i := 0; i+1 < len(n.Content); i += 2 {
						p.Command.Set = append(p.Command.Set, spec.SetCol{Column: n.Content[i].Value, Value: strings.TrimSpace(n.Content[i+1].Value)})
					}
				}
			},
			Check: func(s *spec.Spec) []string { return checkCommand(kind, s) },
			IR: func(s *spec.Spec, api *ir.API) error {
				api.Command = &ir.Command{Op: kind, Table: s.Datasource.Table, NotFound: s.Pattern.NotFoundMessage}
				return nil
			},
		})
	}
}

func checkCommand(kind string, s *spec.Spec) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("pattern "+kind+": "+f, a...)) }
	c := s.Pattern.Command
	if c == nil {
		return []string{"pattern " + kind + ": command is required"}
	}
	if s.Datasource.SQL != "" || len(s.Datasource.Filters) > 0 || len(s.Datasource.Lookups) > 0 || len(s.Datasource.Where) > 0 {
		add("a command writes one table: datasource.sql / filters / lookups / where are for queries")
	}
	switch kind {
	case "insert":
		if len(c.Key) > 0 || c.Lock != nil {
			add("key / lock are for update and delete (insert writes the key columns with write)")
		}
		if len(c.Write) == 0 && len(c.Set) == 0 {
			add("write or set is required")
		}
	case "update":
		if len(c.Key) == 0 {
			add("key is required")
		}
		if len(c.Write) == 0 && len(c.Set) == 0 {
			add("write or set is required (what does the update change?)")
		}
	case "delete":
		if len(c.Key) == 0 {
			add("key is required")
		}
		if len(c.Write) > 0 || len(c.Set) > 0 {
			add("write / set are for insert and update")
		}
	}
	if kind != "insert" && s.Pattern.NotFoundMessage == "" {
		add("not_found_message is required")
	}
	seen := map[string]string{}
	cols := func(what string, bs ...spec.Binding) {
		for _, b := range bs {
			if b.Field == "" || b.Column == "" {
				add("%s: field and column are required", what)
			}
			if prev, dup := seen[b.Column]; dup {
				add("column %s is used twice (%s, %s)", b.Column, prev, what)
			}
			seen[b.Column] = what
		}
	}
	cols("key", c.Key...)
	cols("write", c.Write...)
	if c.Lock != nil {
		cols("lock", *c.Lock)
	}
	for _, x := range c.Set {
		if prev, dup := seen[x.Column]; dup {
			add("column %s is used twice (%s, set)", x.Column, prev)
		}
		seen[x.Column] = "set"
		if x.Value == "" {
			add("set.%s: value is required", x.Column)
		}
	}
	for _, cs := range s.Acceptance.Cases {
		if cs.After == nil {
			add("acceptance.cases[%q]: after is required for commands (the state after the request; unchanged is allowed)", cs.Name)
		}
		if len(cs.Steps) > 0 {
			add("acceptance.cases[%q]: steps are not supported for commands (each case starts from the fixtures)", cs.Name)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 组合命令(R3-2): 一个请求里按顺序执行的多个写操作,一个事务。条件更新、幂等新建是其中的操作。
//
//	种类: 组合命令
//	操作:
//	  - 扣减: { 更新: stock, 键: { sku: 输入.sku }, 设置: { available: "available + 输入.delta" },
//	            条件: "available + 输入.delta >= 0", 不满足: INSUFFICIENT_STOCK, 找不到时: "…" }
//	  - 记录: { 新建: stock_adjustments, 值: { sku: 扣减.sku, delta: 输入.delta } }
//	  - 支付: { 幂等新建: payments, 键: [order_id], 值: { … } }
//	输出:
//	  available: { 说明: …, 来自: 扣减.available }
//	  created:   { 说明: …, 来自: 支付.新建 }
// ---------------------------------------------------------------------------

// OpKinds: 操作的写法 → 内部名。
var OpKinds = map[string]string{"新建": "insert", "幂等新建": "insert_idempotent", "更新": "update", "删除": "delete"}

// OpKeys: 一个操作可以写的键(除了种类本身)。
var OpKeys = []string{"键", "值", "设置", "条件", "不满足", "乐观锁", "找不到时", "仅当", "每项"}

// SkipWord: 不满足: 跳过(R4: 条件不成立不是错误,这个操作不做)。UpdatedWord: 仅当 操作名.已更新(前面的更新真的更新了)。
const (
	SkipWord    = "跳过"
	UpdatedWord = "已更新"
)

// CreatedWord: 输出 来自 操作名.新建 = 幂等新建是否真的插入了。
const CreatedWord = "新建"

func init() {
	Register(&Exit{
		Kind: "command", Name: "组合命令", Cardinality: "one", Command: true,
		DesignKeys: []string{"操作", "发出"},
		Parse: func(d Design, p *spec.Pattern) {
			parseOps(d, p)
			p.Command.Emit = parseEmits(d, d.Node("发出"), "发出")
		},
		Check: checkComposite,
		IR: func(s *spec.Spec, api *ir.API) error {
			api.Command = &ir.Command{Op: "command"}
			return nil
		},
	})
}

// parseOps: 操作的列表 → spec(表达式保持设计稿的写法;设计编译器解释名字、换掉枚举的值名)。
func parseOps(d Design, p *spec.Pattern) {
	if p.Command == nil {
		p.Command = &spec.Command{}
	}
	n := d.Node("操作")
	if n == nil || n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		d.Err("组合命令: 缺少 操作(写操作的列表: - 名字: { 新建 / 幂等新建 / 更新 / 删除: 表, … })")
		return
	}
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode || len(item.Content) != 2 || item.Content[1].Kind != yaml.MappingNode {
			d.Err("操作 第 %d 行: 每个操作写成 - 名字: { 更新: 表, … }", item.Line)
			continue
		}
		name, body := item.Content[0].Value, item.Content[1]
		op := spec.CommandOp{Name: name}
		for i := 0; i+1 < len(body.Content); i += 2 {
			k, v := body.Content[i].Value, body.Content[i+1]
			if kind, ok := OpKinds[k]; ok {
				if op.Kind != "" {
					d.Err("操作 %s: 新建 / 幂等新建 / 更新 / 删除 只能写一个", name)
				}
				op.Kind, op.Table = kind, strings.TrimSpace(v.Value)
				continue
			}
			switch k {
			case "键":
				if v.Kind == yaml.SequenceNode { // 幂等新建: 键的列
					for _, c := range v.Content {
						op.Conflict = append(op.Conflict, strings.TrimSpace(c.Value))
					}
					continue
				}
				op.Key = append(op.Key, colExprs(d, name, k, v)...)
			case "值", "设置":
				op.Values = append(op.Values, colExprs(d, name, k, v)...)
			case "乐观锁":
				if xs := colExprs(d, name, k, v); len(xs) == 1 {
					op.Lock = &xs[0]
				} else {
					d.Err("操作 %s: 乐观锁 写成 { 版本列: 输入.version }", name)
				}
			case "条件":
				op.Cond = strings.TrimSpace(v.Value)
			case "不满足":
				if u := strings.TrimSpace(v.Value); u == SkipWord {
					op.Skip = true
				} else {
					op.Unmet = u
				}
			case "仅当":
				op.Only = strings.TrimSpace(v.Value)
			case "找不到时":
				op.NotFound = v.Value
			case "每项":
				list, ok := strings.CutPrefix(strings.TrimSpace(v.Value), "输入.")
				if !ok || list == "" {
					d.Err("操作 %s: 每项 写成 输入.列表(组列表的输入)", name)
				}
				op.Each = list
			default:
				d.Err("操作 %s: 不认识的键 %s(可以写: 新建 / 幂等新建 / 更新 / 删除 + %s)", name, k, strings.Join(OpKeys, " / "))
			}
		}
		if op.Kind == "" {
			d.Err("操作 %s: 要写 新建 / 幂等新建 / 更新 / 删除 中的一个(值 = 表)", name)
		}
		p.Command.Ops = append(p.Command.Ops, op)
	}
}

func colExprs(d Design, op, key string, v *yaml.Node) []spec.ColExpr {
	if v.Kind != yaml.MappingNode {
		d.Err("操作 %s: %s 写成 { 列: 值 }", op, key)
		return nil
	}
	var out []spec.ColExpr
	for i := 0; i+1 < len(v.Content); i += 2 {
		out = append(out, spec.ColExpr{Column: v.Content[i].Value, Expr: strings.TrimSpace(v.Content[i+1].Value)})
	}
	return out
}

// reasonCode: 业务原因码的写法(大写英文字母、数字、_)。
func reasonCode(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// IsReasonCode: 例子的 错误: 写的是业务原因码(不是错误码)。
func IsReasonCode(c *spec.Command, s string) bool {
	if c == nil {
		return false
	}
	for _, op := range c.Ops {
		if op.Unmet == s {
			return true
		}
	}
	return false
}

func checkComposite(s *spec.Spec) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf("pattern command: "+f, a...)) }
	c := s.Pattern.Command
	if c == nil || len(c.Ops) == 0 {
		return []string{"pattern command: ops are required"}
	}
	if len(c.Key)+len(c.Write)+len(c.Set) > 0 || c.Lock != nil {
		add("key / write / lock / set are for 新建 / 按键更新 / 按键删除 (a composite command writes with ops)")
	}
	if s.Datasource.SQL != "" || s.Datasource.Table != "" || len(s.Datasource.Filters)+len(s.Datasource.Lookups)+len(s.Datasource.Where) > 0 {
		add("datasource is for queries (the ops name their tables)")
	}
	names := map[string]bool{}
	codes := map[string]bool{"OK": true, "NOT_FOUND": true, "ABORTED": true, "ALREADY_EXISTS": true, "INVALID_ARGUMENT": true,
		"FAILED_PRECONDITION": true, "INTERNAL": true, "UNAVAILABLE": true}
	for i, op := range c.Ops {
		if op.Name == "" || names[op.Name] {
			add("ops[%d]: name %q is empty or used twice", i, op.Name)
		}
		names[op.Name] = true
		if op.Table == "" {
			add("ops[%d] %s: table is required", i, op.Name)
		}
		switch op.Kind {
		case "insert", "insert_idempotent":
			if len(op.Key) > 0 || op.Lock != nil || op.Cond != "" || op.NotFound != "" {
				add("%s: key / lock / cond / not_found are for update and delete", op.Name)
			}
			if len(op.Values) == 0 {
				add("%s: values are required", op.Name)
			}
			if (op.Kind == "insert_idempotent") != (len(op.Conflict) > 0) {
				add("%s: conflict (the idempotency key) is required for insert_idempotent and only for it", op.Name)
			}
		case "update", "delete":
			if len(op.Key) == 0 {
				add("%s: key is required", op.Name)
			}
			if op.NotFound == "" {
				add("%s: not_found is required", op.Name)
			}
			if op.Kind == "update" && len(op.Values) == 0 {
				add("%s: values are required (what does the update change?)", op.Name)
			}
			if op.Kind == "delete" && (len(op.Values) > 0 || op.Cond != "") {
				add("%s: values / cond are for update", op.Name)
			}
			if len(op.Conflict) > 0 {
				add("%s: conflict is for insert_idempotent", op.Name)
			}
		default:
			add("%s: kind %q (insert / insert_idempotent / update / delete)", op.Name, op.Kind)
		}
		if (op.Cond != "") != (op.Unmet != "" || op.Skip) {
			add("%s: cond and unmet (or skip) go together (a condition needs the business reason code of its failure)", op.Name)
		}
		if op.Skip && op.Unmet != "" {
			add("%s: skip and unmet are exclusive", op.Name)
		}
		if op.Unmet != "" && (!reasonCode(op.Unmet) || codes[op.Unmet]) {
			add("%s: unmet %q must be an UPPER_SNAKE business reason code (not an error code)", op.Name, op.Unmet)
		}
	}
	out = append(out, checkMessaging(c, "pattern command: ")...)
	for _, o := range c.Output {
		if !names[o.Op] {
			add("output %s: op %q does not exist", o.Field, o.Op)
		}
	}
	for _, cs := range s.Acceptance.Cases {
		if cs.After == nil {
			add("acceptance.cases[%q]: after is required for commands", cs.Name)
		}
		if len(cs.Steps) > 0 {
			add("acceptance.cases[%q]: steps are not supported for commands", cs.Name)
		}
	}
	return out
}
