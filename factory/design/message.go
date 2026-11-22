package design

// 消息处理器(R4-2 / R4-3)和 发出(R4-1)在设计编译器里的部分:
//   - 处理器的"请求" = 收到的契约的 data(字段来自契约,不在设计稿里写 输入;列表 = 组列表 → <API><元素> message)
//   - 处理器不是 HTTP 的 RPC: proto 里另外一个服务 <领域>MessageHandlerService 列出处理器(只是类型的说明,不挂载;
//     入口 = RabbitMQ 的队列 <服务>.<消息类型>),请求 = <API>Request(契约的 data),响应 = 空的 <API>Response
//   - 发出的契约的 data = <Type>V<N> message(protojson 和查询 API 同一种表示)

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"apifactory/factory/exits"
	"apifactory/factory/spec"
)

// handlerInputs: 消息处理器的输入 = 收到的契约的字段。
func (c *ctx) handlerInputs(a *API, e *errs) {
	cmd := a.Pattern.Command
	if cmd == nil || cmd.Receive == nil {
		return
	}
	if len(a.Inputs) > 0 {
		e.add(a.Inputs[0].Line, "消息处理: 不写 输入(处理器的输入 = 收到的消息的字段,来自契约 %s)", cmd.Receive.Type)
	}
	ct := c.contracts.byTyp[cmd.Receive.Type]
	if ct == nil {
		e.add(1, "收到 %s: 没有这个消息契约(这个领域的 %s,或者共享目录 %s 里别的服务发布的)", cmd.Receive.Type, ContractsFile, c.cfg.Contracts)
		return
	}
	cmd.Receive.Version = ct.Version
	a.Inputs = inputsOf(ct.Fields)
	if cmd.Each != "" {
		var list *spec.ContractField
		for i := range ct.Fields {
			if ct.Fields[i].Name == cmd.Each {
				list = &ct.Fields[i]
			}
		}
		switch {
		case list == nil || list.Type != ListType:
			e.add(1, "每项: %s 不是契约 %s 的列表字段", cmd.Each, ct.Type)
		case len(list.Unique) == 0:
			e.add(1, "每项: 契约 %s 的列表 %s 要写 唯一(元素的键: 重复的元素是毒消息;执行前按它排序 = 加行锁的顺序)", ct.Type, cmd.Each)
		}
	}
	rc := *ct
	rc.Proto = a.Name + "Request"
	cmd.Contracts = append(cmd.Contracts, rc)
}

// emitContract: 发出的消息类型 → 契约(proto 的 data message 加进这个领域,一次)。
func (c *ctx) emitContract(a *API, typ string, e *errs) *spec.Contract {
	ct := c.contracts.byTyp[typ]
	if ct == nil {
		e.add(1, "发出 %s: 没有这个消息契约(%s 或共享目录 %s)", typ, ContractsFile, c.cfg.Contracts)
		return nil
	}
	name := protoName(ct)
	if _, done := c.msgByNm[name]; !done {
		fake := &API{Name: name}
		m := &message{Name: name, Desc: fmt.Sprintf("消息 %s v%d 的 data(%s)", ct.Type, ct.Version, ct.Desc)}
		for _, in := range inputsOf(ct.Fields) {
			if f := c.inputField(fake, in, e); f != nil {
				c.uniqueRule(ct.Fields, f)
				m.Fields = append(m.Fields, f)
			}
		}
		c.addMessage(m)
	}
	cmd := a.Pattern.Command
	for _, x := range cmd.Contracts {
		if x.Type == typ && x.Proto == name {
			return ct
		}
	}
	rc := *ct
	rc.Proto = name
	cmd.Contracts = append(cmd.Contracts, rc)
	return ct
}

// uniqueRule: 列表的 唯一 → protovalidate 的 CEL(重复的元素 = 参数错误 / 毒消息)。
func (c *ctx) uniqueRule(fs []spec.ContractField, f *field) {
	for _, cf := range fs {
		if cf.Name != f.Name || len(cf.Unique) == 0 {
			continue
		}
		var parts []string
		for _, u := range cf.Unique {
			parts = append(parts, "string(x."+u+")")
		}
		expr := fmt.Sprintf("this.map(x, %s).unique()", strings.Join(parts, " + '|' + "))
		f.Rules = append(f.Rules, fmt.Sprintf("cel = {id: \"%s.unique\", message: \"%s: %s must be unique\", expression: \"%s\"}",
			f.Name, f.Name, strings.Join(cf.Unique, "+"), strings.ReplaceAll(expr, `"`, `\"`)))
	}
}

// handlerRPC: 处理器在 MessageHandlerService 里的一行。
func (c *ctx) handlerRPC(a *API, req, resp *message) string {
	cmd := a.Pattern.Command
	return fmt.Sprintf("  // %s(收到 %s v%d;入口 = RabbitMQ 的队列,不是 HTTP)\n  rpc %s(%s) returns (%s);", oneLine(a.Desc), cmd.Receive.Type, cmd.Receive.Version, a.Name, req.Name, resp.Name)
}

// handlerCase: 消息处理器的一个用例: 结果(成功 / 业务结果的名字 / 死信 / 毒消息)+ 之后(必填)。
func (c *ctx) handlerCase(a *API, n *yaml.Node) (spec.Expect, *spec.After, error) {
	var ex spec.Expect
	if get(n, "错误") != nil {
		return ex, nil, fmt.Errorf("第 %d 行: 消息处理器的用例写 结果: 成功 / 业务结果 / 死信 / 毒消息(没有 错误)", n.Line)
	}
	res := strings.TrimSpace(str(n, "结果"))
	code, ok := exits.HandlerOutcomes[res]
	if !ok {
		for _, r := range a.Pattern.Command.Results {
			if r.Name == res && r.Name != spec.SuccessResult {
				code, ok = res, true
				if r.DLQ {
					code = "DLQ:" + res
				}
			}
		}
	}
	if !ok {
		return ex, nil, fmt.Errorf("第 %d 行: 结果 %q: 写 成功 / 结果 里的业务结果的名字 / 死信 / 毒消息", n.Line, res)
	}
	ex.Code = code
	// 之后: 和命令同一种写法
	_, after, err := c.commandCase(a, &message{}, withKey(n, "结果", "{}"))
	return ex, after, err
}

// withKey: n 的副本,键 k 换成 v(commandCase 要 结果 是 { … })。
func withKey(n *yaml.Node, k, v string) *yaml.Node {
	cp := *n
	cp.Content = nil
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == k {
			continue
		}
		cp.Content = append(cp.Content, n.Content[i], n.Content[i+1])
	}
	var val yaml.Node
	_ = yaml.Unmarshal([]byte(v), &val)
	cp.Content = append(cp.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, val.Content[0])
	return &cp
}

// emittedCase: 用例的 发出(- 类型: { 字段: 值 })。有 发出 的命令 / 消息处理器必须写(什么都不发 = [])。
func (c *ctx) emittedCase(a *API, n *yaml.Node) (*[]spec.EmittedMsg, error) {
	cmd := a.Pattern.Command
	emits := cmd != nil && (len(cmd.Emit) > 0 || a.Kind == "message")
	node := get(n, "发出")
	switch {
	case node == nil && emits:
		return nil, fmt.Errorf("第 %d 行: 用例要写 发出(写进 outbox 的消息;什么都不发写 [])", n.Line)
	case node == nil:
		return nil, nil
	case !emits:
		return nil, fmt.Errorf("第 %d 行: 发出 只能用于 有 发出 的组合命令 / 消息处理", n.Line)
	case node.Kind != yaml.SequenceNode:
		return nil, fmt.Errorf("第 %d 行: 发出 写成列表: - 消息类型: { 字段: 值 }", node.Line)
	}
	out := []spec.EmittedMsg{}
	for _, item := range node.Content {
		if item.Kind != yaml.MappingNode || len(item.Content) != 2 {
			return nil, fmt.Errorf("第 %d 行: 发出 的每条写成 - 消息类型: { 字段: 值 }", item.Line)
		}
		var data map[string]any
		if err := item.Content[1].Decode(&data); err != nil {
			return nil, fmt.Errorf("第 %d 行: %v", item.Line, err)
		}
		out = append(out, spec.EmittedMsg{Type: strings.TrimSpace(item.Content[0].Value), Data: data})
	}
	return &out, nil
}
