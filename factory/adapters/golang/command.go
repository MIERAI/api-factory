package golang

// 命令(命令工厂,R3-1)的 Go handler: 校验(protovalidate,平台的拦截器)→ 写操作作为数据(txn.Op)→
// 运行时的事务执行器执行(一个事务,READ COMMITTED)→ 失败的原因 → 错误码(writeError,每个服务一个 commands_gen.go)。
// 全部确定性生成,AI 不写。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"apifactory/factory/exits"
	"apifactory/factory/ir"
)

func init() {
	for _, k := range []string{"insert", "update", "delete"} {
		handlerGens[k] = genCommand
	}
}

var txnKinds = map[string]string{"insert": "txn.Insert", "update": "txn.Update", "delete": "txn.Delete"}

func genCommand(g *genCtx) (string, error) {
	cmd := g.api.Command
	in, out := &g.api.Request, &g.api.Response
	g.use(runtimeModule+"/txn", "")
	for _, b := range append(append([]ir.CommandCol{}, cmd.Key...), cmd.Write...) { // 枚举的请求字段 → 列(映射按列查)
		g.filterCol[norm(b.Field)] = b.Column
	}

	// 值: 必填 → 直接写;可以不传(新建)→ 不传时不写这一列(数据库的默认值)
	var fixed, optional []string
	value := func(b ir.CommandCol) error {
		f := in.Field(b.Field)
		if f == nil {
			return fmt.Errorf("command: %s is not a request field", b.Field)
		}
		goType, ok := goOfSQL(b.Type)
		if !ok {
			return fmt.Errorf("command: column %s: type %s is not supported", b.Column, b.Type)
		}
		if f.Optional && f.Kind != "enum" {
			e, err := g.reqExpr(f, "*"+goType)
			if err != nil {
				return err
			}
			optional = append(optional, fmt.Sprintf("if v := %s; v != nil {\n\t\tvalues = append(values, txn.Col{Name: %q, Value: *v})\n\t}", e, b.Column))
			return nil
		}
		if f.Optional { // 枚举: 不传 = nil
			m, err := g.enumFor(b.Column)
			if err != nil {
				return err
			}
			optional = append(optional, fmt.Sprintf("if m.%s != nil {\n\t\tvalues = append(values, txn.Col{Name: %q, Value: %sToDB(m.Get%s())})\n\t}", goCamelCase(f.Name), b.Column, m.Helper, goCamelCase(f.Name)))
			return nil
		}
		e, err := g.reqExpr(f, goType)
		if err != nil {
			return err
		}
		fixed = append(fixed, fmt.Sprintf("{Name: %q, Value: %s},", b.Column, e))
		return nil
	}
	for _, b := range cmd.Write {
		if err := value(b); err != nil {
			return "", err
		}
	}
	for _, s := range cmd.Set {
		v, err := g.setExpr(s)
		if err != nil {
			return "", err
		}
		fixed = append(fixed, fmt.Sprintf("{Name: %q, Value: %s},", s.Column, v))
	}
	if cmd.Op == "update" && cmd.Lock != nil { // 乐观锁: 版本 + 1(写在生成代码里: 漏掉了变异测试能抓到)
		fixed = append(fixed, fmt.Sprintf("txn.Incr1(%q),", cmd.Lock.Column))
	}

	var keys []string
	for _, b := range cmd.Key {
		f := in.Field(b.Field)
		goType, ok := goOfSQL(b.Type)
		if f == nil || !ok {
			return "", fmt.Errorf("command: key %s: no request field or unsupported type %s", b.Field, b.Type)
		}
		e, err := g.reqExpr(f, goType)
		if err != nil {
			return "", err
		}
		keys = append(keys, fmt.Sprintf("{Name: %q, Value: %s},", b.Column, e))
	}

	// 写之后的行(RETURNING)→ 响应
	var rowFields, returning, into, resLines []string
	for i, c := range cmd.Returning {
		goType, ok := goOfSQL(c.Type)
		if !ok {
			return "", fmt.Errorf("command: returning column %s: type %s is not supported", c.Name, c.Type)
		}
		goType = ptr(goType, !c.NotNull)
		if strings.Contains(goType, "time.") {
			g.use("time", "")
		}
		name := goCamelCase(c.Name)
		rowFields = append(rowFields, fmt.Sprintf("%s %s", name, goType))
		returning = append(returning, fmt.Sprintf("%q", c.Name))
		into = append(into, "&row."+name)
		e, err := g.resExpr("row."+name, goType, &out.Fields[i])
		if err != nil {
			return "", err
		}
		resLines = append(resLines, fmt.Sprintf("%s: %s,", goCamelCase(out.Fields[i].Name), e))
	}

	var op []string
	op = append(op, "Kind: "+txnKinds[cmd.Op]+",", fmt.Sprintf("Table: %q,", cmd.Table))
	if cmd.Op != "delete" {
		op = append(op, "Values: values,")
	}
	if len(keys) > 0 {
		op = append(op, "Key: []txn.Col{\n"+strings.Join(keys, "\n")+"\n},")
	}
	if cmd.Lock != nil {
		f := in.Field(cmd.Lock.Field)
		goType, _ := goOfSQL(cmd.Lock.Type)
		e, err := g.reqExpr(f, goType)
		if err != nil {
			return "", err
		}
		op = append(op, fmt.Sprintf("Version: &txn.Col{Name: %q, Value: %s},", cmd.Lock.Column, e))
	}
	if len(returning) > 0 {
		op = append(op, "Returning: []string{"+strings.Join(returning, ", ")+"},", "Into: []any{"+strings.Join(into, ", ")+"},")
	}
	if cmd.Op != "insert" {
		nf, err := g.message(cmd.NotFound)
		if err != nil {
			return "", err
		}
		op = append(op, "NotFound: "+nf+",")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "// %s %s\nfunc (h *Handler) %s(ctx context.Context, req *connect.Request[%s]) (*connect.Response[%s], error) {\n\tm := req.Msg\n",
		g.api.Method, g.api.Summary, g.api.Method, g.pbType(in), g.pbType(out))
	if cmd.Op != "delete" {
		fmt.Fprintf(&b, "\tvalues := []txn.Col{\n%s\n\t}\n", strings.Join(fixed, "\n"))
		for _, o := range optional {
			fmt.Fprintf(&b, "\t%s\n", o)
		}
	}
	fmt.Fprintf(&b, "\tvar row struct {\n%s\n\t}\n", strings.Join(rowFields, "\n"))
	fmt.Fprintf(&b, "\tif err := h.tx.Run(ctx, txn.Op{\n%s\n\t}); err != nil {\n\t\treturn nil, writeError(err)\n\t}\n", strings.Join(op, "\n"))
	fmt.Fprintf(&b, "\treturn connect.NewResponse(&%s{\n%s\n\t}), nil\n}\n", g.pbType(out), strings.Join(resLines, "\n"))
	return g.header(b.String()), nil
}

// message: 消息里的 {字段} → fmt.Sprintf 的参数(没有占位符 → 字符串常量)。
func (g *genCtx) message(text string) (string, error) {
	var args []string
	var bad error
	format := placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		name := m[1 : len(m)-1]
		if g.api.Request.Field(name) == nil {
			bad = fmt.Errorf("not found message: {%s} is not a request field", name)
		}
		args = append(args, fmt.Sprintf("m.Get%s()", goCamelCase(name)))
		return "%v"
	})
	if bad != nil {
		return "", bad
	}
	if len(args) == 0 {
		return fmt.Sprintf("%q", format), nil
	}
	g.use("fmt", "")
	return fmt.Sprintf("fmt.Sprintf(%q, %s)", format, strings.Join(args, ", ")), nil
}

// setExpr: 设置 的值 → Go 的表达式(请求开始的时刻 = clock.Now(ctx);字面值按列的类型)。
func (g *genCtx) setExpr(s ir.CommandSet) (string, error) {
	if s.Now {
		g.use(runtimeModule+"/clock", "")
		return "clock.Now(ctx)", nil
	}
	goType, ok := goOfSQL(s.Type)
	if !ok {
		return "", fmt.Errorf("command: set %s: type %s is not supported", s.Column, s.Type)
	}
	switch goType {
	case "int32", "int64":
		return fmt.Sprintf("%s(%s)", goType, s.Value), nil
	case "bool":
		return s.Value, nil
	case "string":
		return fmt.Sprintf("%q", s.Value), nil
	}
	return "", fmt.Errorf("command: set %s: a fixed value of type %s is not supported", s.Column, s.Type)
}

// connectCodes: 错误码的名字 → connect 的常量(writeError 用)。
var connectCodes = map[string]string{
	"ABORTED": "connect.CodeAborted", "FAILED_PRECONDITION": "connect.CodeFailedPrecondition", "NOT_FOUND": "connect.CodeNotFound",
	"ALREADY_EXISTS": "connect.CodeAlreadyExists", "INVALID_ARGUMENT": "connect.CodeInvalidArgument",
}

// writeCommandErrors: 有命令的服务: commands_gen.go(写操作失败的原因 → 错误码)。没有命令 → 删除。
func writeCommandErrors(svc service) error {
	path := filepath.Join("internal/handler", svc.Domain, "commands_gen.go")
	if !svc.Commands {
		_ = os.Remove(path)
		return nil
	}
	conflict, ok := connectCodes[exits.ConflictCode]
	if !ok {
		return fmt.Errorf("exits.ConflictCode %s has no connect code", exits.ConflictCode)
	}
	imports, unmet := "", ""
	if svc.Unmets { // 条件更新: 业务原因码 = 消息的开头 + google.rpc.ErrorInfo 的 reason(调用方按它分支)
		imports = "\n\t\"google.golang.org/genproto/googleapis/rpc/errdetails\""
		unmet = fmt.Sprintf(`
	case txn.Unmet: // 条件更新的条件不成立: FAILED_PRECONDITION + 设计稿里声明的业务原因码
		ce := connect.NewError(connect.CodeFailedPrecondition, errors.New(e.Msg))
		if d, derr := connect.NewErrorDetail(&errdetails.ErrorInfo{Reason: e.Code, Domain: %q}); derr == nil {
			ce.AddDetail(d)
		}
		return ce`, svc.ProtoPackage)
	}
	src := fmt.Sprintf(`// Code generated by factory. DO NOT EDIT.

package %s

import (
	"errors"

	"connectrpc.com/connect"
	"github.com/MIERAI/api-factory/runtime/txn"`+imports+`
)

// writeError: 写操作失败的原因(运行时 txn 的分类)→ 错误码(命令工厂设计稿 §3.2 / §3.3)。
// 不是写操作的错误(连接断开、超时 …)原样返回,平台的拦截器转成 UNAVAILABLE / INTERNAL。
func writeError(err error) error {
	var e *txn.Error
	if !errors.As(err, &e) {
		return err
	}
	switch e.Reason {
	case txn.NotFound:
		return connect.NewError(connect.CodeNotFound, errors.New(e.Msg))
	case txn.Conflict: // 乐观锁的版本不一致(⚖️ D1: %s)
		return connect.NewError(%s, errors.New(e.Msg))
	case txn.Unique:
		return connect.NewError(connect.CodeAlreadyExists, errors.New(e.Msg))
	case txn.Check, txn.NotNull: // 请求的校验漏了数据库的规则(运行时计入 write_rejected_total)
		return connect.NewError(connect.CodeInvalidArgument, errors.New(e.Msg))
	case txn.ForeignKey:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(e.Msg))
	case txn.Retry: // 序列化失败 / 死锁,重试之后仍然失败
		return connect.NewError(connect.CodeAborted, errors.New(e.Msg))`+unmet+`
	}
	return err
}
`, svc.Domain, exits.ConflictCode, conflict)
	return writeGo(path, []byte(src))
}
