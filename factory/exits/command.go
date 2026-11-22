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
