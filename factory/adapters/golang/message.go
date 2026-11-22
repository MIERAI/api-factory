package golang

// 消息(R4)的 Go 代码: 组合命令的 发出(写操作和 outbox 同一个事务)、消息处理器(consume.Handler: 解码 + 保存点里的
// 操作 + 结果的分支 + 发出)、服务的 messages_gen.go(发出 / 错误分类的辅助函数)、wiring 的消息部分(拓扑 / 中继 / 消费循环 /
// 进程内投递的测试入口)。全部确定性生成,AI 不写。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"apifactory/factory/command"
	"apifactory/factory/ir"
)

func init() { handlerGens["message"] = genHandler }

// itemValue: 项.x → 当前元素(it)的字段。
func (cg *cmdGen) itemValue(x *ir.CmdExpr) (string, error) {
	if cg.item == nil {
		return "", fmt.Errorf("项.%s outside 每项", x.Name)
	}
	f := cg.item.Field(x.Name)
	if f == nil {
		return "", fmt.Errorf("项.%s: no field %s in %s", x.Name, x.Name, cg.item.Name)
	}
	goType, ok := goOfSQL(x.Type)
	if !ok {
		return "", fmt.Errorf("项.%s: type %s is not supported", x.Name, x.Type)
	}
	e, err := cg.g.reqExpr(f, goType)
	if err != nil {
		return "", err
	}
	return strings.Replace(e, "m.", "it.", 1), nil
}

// cmpOperand: 条件的一边 → Go 的值 + 种类(int / text / bool)+ 前提(可能为空的返回值、仅当 没执行的操作)。
func (cg *cmdGen) cmpOperand(x *ir.CmdExpr, ran map[string]bool) (expr, kind, guard string, err error) {
	switch x.Op {
	case "const":
		switch {
		case isIntType(x.Type) || strings.HasPrefix(x.Type, "enum"):
			return x.Value, "int", "", nil
		case x.Type == "text":
			return fmt.Sprintf("%q", x.Value), "text", "", nil
		case x.Type == "bool":
			return x.Value, "bool", "", nil
		case x.Type == "numeric":
			return fmt.Sprintf("%q", x.Value), "dec", "", nil
		}
	case "input", "item", "env":
		v, err := cg.value(x)
		if err != nil {
			return "", "", "", err
		}
		return v, kindOfSQL(x.Type), "", nil
	case "ref":
		n := cg.idx[x.Ref]
		op := cg.g.api.Command.Ops[n-1]
		v := fmt.Sprintf("r%d.%s", n, goCamelCase(x.Name))
		var guards []string
		if op.Only != "" {
			guards = append(guards, fmt.Sprintf("ran%d", n))
			ran[x.Ref] = true
		}
		for _, c := range op.Returning {
			if c.Name == x.Name && !c.NotNull {
				guards = append(guards, v+" != nil")
				v = "*" + v
			}
		}
		return v, kindOfSQL(x.Type), strings.Join(guards, " && "), nil
	}
	return "", "", "", fmt.Errorf("condition: %s (%s) cannot be evaluated in Go (only integers / text / booleans / enums)", x.Op, x.Type)
}

// cond: 条件 → Go 的布尔表达式(结果的 当、如果 的条件)。空值 → 不成立。
func (cg *cmdGen) cond(x *ir.CmdExpr) (string, error) { return cg.condRan(x, map[string]bool{}) }

func (cg *cmdGen) condRan(x *ir.CmdExpr, ran map[string]bool) (string, error) {
	if x.Op == "and" {
		var parts []string
		for _, a := range x.Args {
			p, err := cg.condRan(a, ran)
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return "(" + strings.Join(parts, " && ") + ")", nil
	}
	a, ka, ga, err := cg.cmpOperand(x.Args[0], ran)
	if err != nil {
		return "", err
	}
	b, kb, gb, err := cg.cmpOperand(x.Args[1], ran)
	if err != nil {
		return "", err
	}
	if ka != kb || ka == "" {
		return "", fmt.Errorf("condition %s: %s vs %s", x.Op, ka, kb)
	}
	op := map[string]string{"=": "==", "<>": "!="}[x.Op]
	if op == "" {
		op = x.Op
	}
	if ka != "int" && ka != "dec" && op != "==" && op != "!=" {
		return "", fmt.Errorf("condition: %s only = / <>", ka)
	}
	e := fmt.Sprintf("%s %s %s", a, op, b)
	switch ka {
	case "int":
		e = fmt.Sprintf("int64(%s) %s int64(%s)", a, op, b)
	case "dec": // 十进制: 按数值比较(文本是 protojson / 数据库的写法)
		e = fmt.Sprintf("decCmp(%s, %s) %s 0", a, b, op)
	}
	var guards []string
	for _, gd := range []string{ga, gb} {
		if gd != "" {
			guards = append(guards, gd)
		}
	}
	if len(guards) > 0 {
		return "(" + strings.Join(guards, " && ") + " && " + e + ")", nil
	}
	return "(" + e + ")", nil
}

func isIntType(t string) bool { return t == "int2" || t == "int4" || t == "int8" }

func kindOfSQL(t string) string {
	switch {
	case isIntType(t) || strings.HasPrefix(t, "enum"):
		return "int"
	case t == "text":
		return "text"
	case t == "bool":
		return "bool"
	case t == "numeric":
		return "dec"
	}
	return ""
}

// contract: 发出的消息类型 → 契约。
func (cg *cmdGen) contract(typ string) (*ir.Contract, error) {
	c := cg.g.api.Command
	for i := range c.Contracts {
		if c.Contracts[i].Type == typ {
			return &c.Contracts[i], nil
		}
	}
	return nil, fmt.Errorf("emit %s: no contract", typ)
}

// emits: 发出的消息 → 组装 data + 写 outbox 的语句(最后一句 return)。indent = 缩进。
func (cg *cmdGen) emits(list []ir.CommandEmit, indent string) (string, error) {
	g := cg.g
	if len(list) == 0 {
		return indent + "return nil\n", nil
	}
	g.use(runtimeModule+"/message", "")
	g.use("fmt", "")
	var b strings.Builder
	var msgs []string
	for i, em := range list {
		ct, err := cg.contract(em.Type)
		if err != nil {
			return "", err
		}
		var fields []string
		for _, v := range em.Values {
			cf := ct.Message.Field(v.Field)
			if cf == nil {
				return "", fmt.Errorf("emit %s: no field %s in %s", em.Type, v.Field, ct.Message.Name)
			}
			e, err := cg.emitValue(v, cf)
			if err != nil {
				return "", fmt.Errorf("emit %s.%s: %w", em.Type, v.Field, err)
			}
			fields = append(fields, fmt.Sprintf("%s: %s,", goCamelCase(v.Field), e))
		}
		kind := "message.Event"
		if ct.Kind == "command" {
			kind = "message.Command"
		}
		fmt.Fprintf(&b, "%sd%d := &%s{ // %s v%d 的 data\n%s\n%s}\n", indent, i+1, g.pbType(&ct.Message), em.Type, ct.Version, strings.Join(fields, "\n"), indent)
		msgs = append(msgs, fmt.Sprintf("emitMsg(%s, %q, %d, fmt.Sprint(d%d.Get%s()), d%d)", kind, em.Type, ct.Version, i+1, goCamelCase(ct.Subject), i+1))
	}
	fmt.Fprintf(&b, "%sreturn h.emit(ctx, tx,\n%s,\n%s)\n", indent, strings.Join(msgs, ",\n"), indent)
	return b.String(), nil
}

// emitValue: data 的一个字段 ← 表达式(请求的字段 / 操作的返回值 / 常量 / 现在 / 消息的 id;列表 = 逐个元素复制)。
func (cg *cmdGen) emitValue(v ir.EmitVal, cf *ir.Field) (string, error) {
	g, x := cg.g, v.Expr
	if v.List {
		src := g.api.Request.Field(x.Name)
		if src == nil || src.Message == nil || cf.Message == nil {
			return "", fmt.Errorf("list %s", x.Name)
		}
		var cp []string
		for _, f := range cf.Message.Fields {
			cp = append(cp, fmt.Sprintf("%s: x.Get%s(),", goCamelCase(f.Name), goCamelCase(f.Name)))
		}
		el := g.pbType(cf.Message)
		return fmt.Sprintf("func() []*%[1]s {\n\tout := make([]*%[1]s, 0, len(m.Get%[2]s()))\n\tfor _, x := range m.Get%[2]s() {\n\t\tout = append(out, &%[1]s{%[3]s})\n\t}\n\treturn out\n}()",
			el, goCamelCase(x.Name), strings.Join(cp, " ")), nil
	}
	switch x.Op {
	case "input":
		f := g.api.Request.Field(x.Name)
		if f == nil || f.Kind != cf.Kind {
			return "", fmt.Errorf("输入.%s: kind differs from the contract field", x.Name)
		}
		if f.Optional {
			return "m." + goCamelCase(f.Name), nil
		}
		return fmt.Sprintf("m.Get%s()", goCamelCase(f.Name)), nil
	case "ref":
		n := cg.idx[x.Ref]
		var col *ir.Column
		for i, c := range g.api.Command.Ops[n-1].Returning {
			if c.Name == x.Name {
				col = &g.api.Command.Ops[n-1].Returning[i]
			}
		}
		if col == nil {
			return "", fmt.Errorf("%s.%s is not returned", x.Ref, x.Name)
		}
		goType, _ := goOfSQL(col.Type)
		return g.resExpr(fmt.Sprintf("r%d.%s", n, goCamelCase(x.Name)), ptr(goType, !col.NotNull), cf)
	case "now":
		g.use(runtimeModule+"/clock", "")
		return g.resExpr("clock.Now(ctx)", "time.Time", cf)
	case "env":
		return "env.ID", nil
	case "const":
		return constLit(x)
	}
	return "", fmt.Errorf("%s cannot be emitted (输入 / 操作名.列 / 常量 / 现在 / 信封.id)", x.Op)
}

// genHandler: 消息处理器 → func (h *Handler) X(ctx, tx, env) error(consume.Handler)。
func genHandler(g *genCtx) (string, error) {
	cmd := g.api.Command
	in := &g.api.Request
	g.use(runtimeModule+"/txn", "")
	g.use(runtimeModule+"/consume", "")
	g.use(runtimeModule+"/message", "")
	g.use("github.com/jackc/pgx/v5", "")
	delete(g.imports, "connectrpc.com/connect")
	cg := newCmdGen(g)
	ind := "\t\t"
	if cmd.Each != "" {
		lf := in.Field(cmd.Each)
		if lf == nil || lf.Message == nil {
			return "", fmt.Errorf("每项 %s: not a list of messages", cmd.Each)
		}
		cg.item = lf.Message
		ind = "\t\t\t"
	}
	// 结果的 当 引用的 仅当 的操作: 要知道执行了没有(ran)
	ran := map[string]bool{}
	for _, r := range cmd.Results {
		if r.When == nil {
			continue
		}
		if _, err := cg.condRan(r.When, ran); err != nil {
			return "", fmt.Errorf("result %s: %w", resultName(r), err)
		}
	}
	var body strings.Builder
	var outerVars []string
	var opsCode strings.Builder
	for i, op := range cmd.Ops {
		n := i + 1
		lit, vars, pre, err := cg.op(op, n)
		if err != nil {
			return "", err
		}
		if len(pre) > 0 {
			return "", fmt.Errorf("op %s: optional inputs in a handler op are not supported", op.Name)
		}
		if ran[op.Name] {
			vars = append(vars, fmt.Sprintf("var ran%d bool // %s: 执行了(仅当)", n, op.Name))
		}
		if op.Skip || op.Updated {
			vars = append(vars, fmt.Sprintf("var updated%d bool // %s: 真的更新了", n, op.Name))
		}
		if cmd.Each != "" {
			for _, v := range vars {
				fmt.Fprintf(&opsCode, "%s%s\n", ind, v)
			}
		} else {
			outerVars = append(outerVars, vars...)
		}
		pad := ind
		if op.Only != "" {
			flag := "created"
			if op.OnlyFlag == "已更新" {
				flag = "updated"
			}
			fmt.Fprintf(&opsCode, "%sif %s%d { // 仅当 %s.%s\n", ind, flag, cg.idx[op.Only], op.Only, op.OnlyFlag)
			pad = ind + "\t"
			if ran[op.Name] {
				fmt.Fprintf(&opsCode, "%sran%d = true\n", pad, n)
			}
		}
		switch {
		case op.Skip: // 条件不成立 = 跳过(Into = 现在的行)
			fmt.Fprintf(&opsCode, "%sif err := txn.RunIn(ctx, tx, %s\n%s); err == nil {\n%s\tupdated%d = true\n%s} else if txn.ReasonOf(err) != txn.Unmet { // 条件不成立: 跳过\n%s\treturn err\n%s}\n",
				pad, lit, pad, pad, n, pad, pad, pad)
		default:
			fmt.Fprintf(&opsCode, "%sif err := txn.RunIn(ctx, tx, %s\n%s); err != nil {\n%s\treturn err\n%s}\n", pad, lit, pad, pad, pad)
			if op.Updated {
				fmt.Fprintf(&opsCode, "%supdated%d = true\n", pad, n)
			}
		}
		if op.Only != "" {
			fmt.Fprintf(&opsCode, "%s}\n", ind)
		}
	}
	fmt.Fprintf(&body, "// %s %s\n// 消息处理器(consume.Handler): 平台在同一个事务里做去重,提交之后才确认;重试 / 死信也由平台做\nfunc (h *Handler) %s(ctx context.Context, tx pgx.Tx, env message.Envelope) error {\n",
		g.api.Method, g.api.Summary, g.api.Method)
	fmt.Fprintf(&body, "\tm := &%s{}\n\tif err := message.DecodeData(env, m); err != nil { // 解不开 / 违反契约 = 毒消息\n\t\treturn consume.Poison(err)\n\t}\n", g.pbType(in))
	for _, v := range outerVars {
		fmt.Fprintf(&body, "\t%s\n", v)
	}
	// 当(任一项): 每项的循环里累计
	for i, r := range cmd.Results {
		if r.When != nil && r.Any {
			fmt.Fprintf(&body, "\tvar any%d bool // 结果 %s: 任一项 %s\n", i+1, resultName(r), formatCond(r))
		}
	}
	if cmd.Each != "" {
		g.use("slices", "")
		g.use("cmp", "")
		var keys []string
		for _, k := range cmd.EachKey {
			keys = append(keys, fmt.Sprintf("cmp.Compare(a.Get%s(), b.Get%s())", goCamelCase(k), goCamelCase(k)))
		}
		cmpExpr := keys[0]
		if len(keys) > 1 {
			cmpExpr = "cmp.Or(" + strings.Join(keys, ", ") + ")"
		}
		fmt.Fprintf(&body, "\titems := slices.Clone(m.Get%s())\n\tslices.SortStableFunc(items, func(a, b *%s) int { return %s }) // 按元素的键排序: 并发的处理按同一个顺序加行锁\n",
			goCamelCase(cmd.Each), g.pbType(cg.item), cmpExpr)
	}
	body.WriteString("\terr := txn.Savepoint(ctx, tx, func(ctx context.Context, tx pgx.Tx) error { // 业务结果: 回滚到这里(去重记录和结果的消息照样提交)\n")
	if cmd.Each != "" {
		body.WriteString("\t\tfor _, it := range items {\n")
	}
	body.WriteString(opsCode.String())
	if cmd.Each != "" {
		for i, r := range cmd.Results {
			if r.When != nil && r.Any {
				c, _ := cg.condRan(r.When, ran)
				fmt.Fprintf(&body, "\t\t\tif %s {\n\t\t\t\tany%d = true\n\t\t\t}\n", c, i+1)
			}
		}
		body.WriteString("\t\t}\n")
	}
	for i, r := range cmd.Results { // 非 成功 的 当: 保存点里判定(成立 → 回滚,结果 = 名字)
		if r.When == nil || r.Name == "" {
			continue
		}
		c := fmt.Sprintf("any%d", i+1)
		if !r.Any {
			var err error
			if c, err = cg.condRan(r.When, ran); err != nil {
				return "", err
			}
		}
		fmt.Fprintf(&body, "\t\tif %s { // 结果 %s\n\t\t\treturn resultError(%q)\n\t\t}\n", c, r.Name, r.Name)
	}
	body.WriteString("\t\treturn nil\n\t})\n")
	// 业务结果的分支
	var cases []string
	for _, r := range cmd.Results {
		if r.Name == "" {
			continue
		}
		var cb strings.Builder
		fmt.Fprintf(&cb, "\tcase %q:\n", r.Name)
		if r.DLQ {
			g.use("errors", "")
			fmt.Fprintf(&cb, "\t\treturn consume.Poison(errors.New(%q)) // 处理: 死信(不提交,需要人处理)\n", r.Name+": "+formatCond(r))
		} else {
			fmt.Fprintf(&cb, "\t\tconsume.Business(ctx, %q)\n", r.Name)
			e, err := cg.emits(r.Emits, "\t\t")
			if err != nil {
				return "", err
			}
			cb.WriteString(e)
		}
		cases = append(cases, cb.String())
	}
	if len(cases) > 0 {
		fmt.Fprintf(&body, "\tresult, err := handlerResult(err)\n\tif err != nil {\n\t\treturn err\n\t}\n\tswitch result {\n%s\t}\n", strings.Join(cases, ""))
	} else {
		body.WriteString("\tif _, err := handlerResult(err); err != nil {\n\t\treturn err\n\t}\n")
	}
	// 成功(分支: 按顺序判定 当)
	done := false
	for _, r := range cmd.Results {
		if r.Name != "" {
			continue
		}
		if r.When != nil {
			c, err := cg.cond(r.When)
			if err != nil {
				return "", err
			}
			e, err := cg.emits(r.Emits, "\t\t")
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&body, "\tif %s { // 成功: 当 %s\n%s\t}\n", c, formatCond(r), e)
			continue
		}
		e, err := cg.emits(r.Emits, "\t")
		if err != nil {
			return "", err
		}
		body.WriteString(e)
		done = true
		break
	}
	if !done {
		body.WriteString("\treturn nil\n")
	}
	body.WriteString("}\n")
	return g.header(body.String()), nil
}

func resultName(r ir.CommandResult) string {
	if r.Name == "" {
		return "成功"
	}
	return r.Name
}

func formatCond(r ir.CommandResult) string {
	if r.When == nil {
		return ""
	}
	return command.Format(r.When)
}

// writeMessaging: 有消息(发出 / 消息处理器)的服务: messages_gen.go。没有 → 删除。
func writeMessaging(svc service) error {
	path := filepath.Join("internal/handler", svc.Domain, "messages_gen.go")
	if !svc.Messages {
		_ = os.Remove(path)
		return nil
	}
	src := fmt.Sprintf(`// Code generated by factory. DO NOT EDIT.

package %s

import (
	"context"
	"errors"
	"math/big"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/MIERAI/api-factory-runtime/consume"
	"github.com/MIERAI/api-factory-runtime/message"
	"github.com/MIERAI/api-factory-runtime/outbox"
	"github.com/MIERAI/api-factory-runtime/txn"
)

// emit: 发出的消息写进 outbox(调用方的事务里: 回滚 = 没有消息;提交之后由中继发到 RabbitMQ)。
func (h *Handler) emit(ctx context.Context, tx pgx.Tx, msgs ...outbox.Message) error {
	_, err := h.out.Write(ctx, tx, msgs...)
	return err
}

// emitMsg: 一条消息(data = 契约的 proto 消息,protojson: 和查询 API 同一种表示)。
func emitMsg(kind message.Kind, typ string, version int, subject string, data proto.Message) outbox.Message {
	return outbox.Message{Kind: kind, Type: typ, Version: version, Subject: subject, Data: protoData{data}}
}

// protoData: data 的 JSON(encoding/json 再压缩一次: protojson 的输出有意不稳定,同一条消息的字节要一样)。
type protoData struct{ m proto.Message }

func (d protoData) MarshalJSON() ([]byte, error) { return message.EncodeData(d.m) }

// decCmp: 两个十进制的文本按数值比较(-1 / 0 / 1)。解不开的当成 0(契约的校验已经拒绝了不合法的写法)。
func decCmp(a, b string) int {
	x, ok1 := new(big.Rat).SetString(a)
	y, ok2 := new(big.Rat).SetString(b)
	if !ok1 || !ok2 {
		return 0
	}
	return x.Cmp(y)
}

// resultError: 结果的 当 成立(保存点回滚,走那个结果的分支)。
type resultError string

func (e resultError) Error() string { return "result " + string(e) }

// handlerResult: 保存点的错误 → 业务结果(声明过的: 条件不满足的原因码 / 当 成立的结果)或者消费循环的分类:
// 数据的问题(找不到 / 违反约束)= 毒消息(重试也不会好,进 DLQ);其他(连接、序列化失败、超时、测试的故障注入)= 暂时性(重试)。
func handlerResult(err error) (string, error) {
	if err == nil {
		return "", nil
	}
	var re resultError
	if errors.As(err, &re) {
		return string(re), nil
	}
	var e *txn.Error
	if !errors.As(err, &e) {
		return "", err
	}
	switch e.Reason {
	case txn.Unmet:
		return e.Code, nil
	case txn.NotFound, txn.Unique, txn.Check, txn.NotNull, txn.ForeignKey:
		return "", consume.Poison(err)
	}
	return "", err
}
`, svc.Domain)
	return writeGo(path, []byte(src))
}
