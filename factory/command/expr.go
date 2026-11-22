package command

// 组合命令(R3-2)的表达式: 写入的值(设置 / 值 / 键)和条件更新的条件。设计稿、spec 用同一种写法:
//
//	条件   = 比较 { "并且" 比较 }
//	比较   = 值 ("=" | "<>" | "<" | "<=" | ">" | ">=") 值
//	值     = 项 { ("+" | "-") 项 }
//	项     = 输入.字段 | 操作名.列 | 列 | 数字 | '文本' | 现在 | now() | 名字(枚举的值名,设计稿里)
//
// 例: available + 输入.delta >= 0 并且 status = HELD
//
// 列 = 这个操作的表(update 的对象)的列,值是更新之前的(和 SQL 一样)。操作名.列 = 前面的操作的返回值。
// 空值: 运算的任一边为空 → 空;比较的任一边为空 → 不成立(条件不满足)。
// 只有加减和比较(没有乘除、函数、或者): 写接口的条件几乎都是"够不够"(余额、库存)和"状态是不是 X"。

import (
	"fmt"
	"strings"
	"unicode"

	"apifactory/factory/ir"
)

// InputPrefix: 表达式里请求字段的前缀(输入.字段)。
const InputPrefix = "输入."

// R4(消息处理器,docs/design/r4-events-messages.md §5)的写法:
//
//	消息.字段   收到的消息的 data 的字段(= 处理器的"请求";spec 里规范化成 输入.字段)
//	项.字段     每项: 列表的当前元素的字段
//	信封.id     消息的 id(envelope.id,文本;例: 以事件 id 为主键的通知记录)
//	如果 条件 则 值 否则 值   条件只用 输入 / 项 / 常量(例: 模拟支付 如果 消息.amount <= 100000 则 SUCCEEDED 否则 FAILED)
//	空          空值(只能写进可以为空的列;如果 的分支里用)
const (
	MessagePrefix = "消息."
	ItemPrefix    = "项."
	EnvPrefix     = "信封."
)

var cmpWords = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

// ParseExpr: 表达式的文本 → 没有类型的树(名字的解释交给 Resolve)。cond = 条件(比较 / 并且),否则是值。
func ParseExpr(src string, cond bool) (*ir.CmdExpr, error) {
	toks, err := lexExpr(src)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", src, err)
	}
	p := &exprParser{toks: toks}
	var e *ir.CmdExpr
	if cond {
		e, err = p.cond()
	} else {
		e, err = p.value()
	}
	if err != nil {
		return nil, fmt.Errorf("%q: %w", src, err)
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("%q: %q 之后有多余的内容", src, p.toks[p.pos-1])
	}
	return e, nil
}

type exprParser struct {
	toks []string
	pos  int
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *exprParser) cond() (*ir.CmdExpr, error) {
	var parts []*ir.CmdExpr
	for {
		c, err := p.cmp()
		if err != nil {
			return nil, err
		}
		parts = append(parts, c)
		if p.peek() != "并且" {
			break
		}
		p.pos++
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return &ir.CmdExpr{Op: "and", Args: parts}, nil
}

func (p *exprParser) cmp() (*ir.CmdExpr, error) {
	a, err := p.value()
	if err != nil {
		return nil, err
	}
	op := p.peek()
	if !cmpWords[op] {
		if op == "" {
			return nil, fmt.Errorf("条件不完整: 需要 比较(= <> < <= > >=)")
		}
		return nil, fmt.Errorf("需要 比较(= <> < <= > >=),不是 %q", op)
	}
	p.pos++
	b, err := p.value()
	if err != nil {
		return nil, err
	}
	return &ir.CmdExpr{Op: op, Args: []*ir.CmdExpr{a, b}}, nil
}

func (p *exprParser) value() (*ir.CmdExpr, error) {
	x, err := p.term()
	if err != nil {
		return nil, err
	}
	for p.peek() == "+" || p.peek() == "-" {
		op := p.peek()
		p.pos++
		y, err := p.term()
		if err != nil {
			return nil, err
		}
		x = &ir.CmdExpr{Op: op, Args: []*ir.CmdExpr{x, y}}
	}
	return x, nil
}

func (p *exprParser) term() (*ir.CmdExpr, error) {
	t := p.peek()
	if t == "" {
		return nil, fmt.Errorf("不完整: 缺少值")
	}
	p.pos++
	switch {
	case t == "现在" || t == "now()":
		return &ir.CmdExpr{Op: "now"}, nil
	case strings.HasPrefix(t, "'"):
		return &ir.CmdExpr{Op: "const", Value: strings.ReplaceAll(t[1:len(t)-1], "''", "'"), Type: "text"}, nil
	case isNumberTok(t):
		return &ir.CmdExpr{Op: "const", Value: t}, nil
	case t == "-" && isNumberTok(p.peek()): // 负数
		n := p.peek()
		p.pos++
		return &ir.CmdExpr{Op: "const", Value: "-" + n}, nil
	case strings.HasPrefix(t, InputPrefix) || strings.HasPrefix(t, MessagePrefix):
		name := strings.TrimPrefix(strings.TrimPrefix(t, InputPrefix), MessagePrefix)
		if !isIdent(name) {
			return nil, fmt.Errorf("%q: 输入. / 消息. 后面写字段的名字", t)
		}
		return &ir.CmdExpr{Op: "input", Name: name}, nil
	case strings.HasPrefix(t, ItemPrefix):
		name := strings.TrimPrefix(t, ItemPrefix)
		if !isIdent(name) {
			return nil, fmt.Errorf("%q: 项. 后面写元素的字段名", t)
		}
		return &ir.CmdExpr{Op: "item", Name: name}, nil
	case t == EnvPrefix+"id":
		return &ir.CmdExpr{Op: "env", Name: "id", Type: "text"}, nil
	case strings.HasPrefix(t, EnvPrefix):
		return nil, fmt.Errorf("%q: 信封 只能取 信封.id(消息的 id)", t)
	case t == "空":
		return &ir.CmdExpr{Op: "null"}, nil
	case t == "如果":
		c, err := p.cond()
		if err != nil {
			return nil, err
		}
		if p.peek() != "则" {
			return nil, fmt.Errorf("如果 … 则 … 否则 …: 缺少 则")
		}
		p.pos++
		a, err := p.value()
		if err != nil {
			return nil, err
		}
		if p.peek() != "否则" {
			return nil, fmt.Errorf("如果 … 则 … 否则 …: 缺少 否则")
		}
		p.pos++
		b, err := p.value()
		if err != nil {
			return nil, err
		}
		return &ir.CmdExpr{Op: "if", Args: []*ir.CmdExpr{c, a, b}}, nil
	case strings.Count(t, ".") == 1:
		op, col, _ := strings.Cut(t, ".")
		if !isIdent(op) || !isIdent(col) {
			return nil, fmt.Errorf("%q: 引用前面的操作写成 操作名.列", t)
		}
		return &ir.CmdExpr{Op: "ref", Ref: op, Name: col}, nil
	case isIdent(t):
		return &ir.CmdExpr{Op: "name", Name: t}, nil // 列 或 枚举的值名(Resolve 决定)
	}
	return nil, fmt.Errorf("无法识别 %q", t)
}

func isNumberTok(t string) bool {
	if t == "" {
		return false
	}
	dot := false
	for i, r := range t {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !dot && i > 0:
			dot = true
		default:
			return false
		}
	}
	return !strings.HasSuffix(t, ".")
}

func isIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(unicode.IsLetter(r) || r == '_' || (i > 0 && unicode.IsDigit(r))) {
			return false
		}
	}
	return true
}

func lexExpr(src string) ([]string, error) {
	var out []string
	rs := []rune(strings.TrimSpace(src))
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '\'':
			j := i + 1
			for ; j < len(rs); j++ {
				if rs[j] == '\'' {
					if j+1 < len(rs) && rs[j+1] == '\'' {
						j++
						continue
					}
					break
				}
			}
			if j >= len(rs) {
				return nil, fmt.Errorf("引号没有闭合")
			}
			out = append(out, string(rs[i:j+1]))
			i = j + 1
		case r == '<' || r == '>':
			if i+1 < len(rs) && (rs[i+1] == '=' || (r == '<' && rs[i+1] == '>')) {
				out = append(out, string(rs[i:i+2]))
				i += 2
			} else {
				out = append(out, string(r))
				i++
			}
		case r == '=' || r == '+' || r == '-':
			out = append(out, string(r))
			i++
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_' || rs[j] == '.') {
				j++
			}
			w := string(rs[i:j])
			if w == "now" && j+1 < len(rs) && rs[j] == '(' && rs[j+1] == ')' {
				w, j = "now()", j+2
			}
			if strings.HasPrefix(w, "并且") && w != "并且" { // "并且" 紧接着后面的词
				out = append(out, "并且")
				w = strings.TrimPrefix(w, "并且")
			}
			out = append(out, w)
			i = j
		default:
			return nil, fmt.Errorf("无法识别的字符 %q", r)
		}
	}
	return out, nil
}

// Scope: 解释表达式里的名字(设计编译器 / 编译器实现)。
type Scope struct {
	Table  string                                             // 这个操作的表(update 的列;insert 没有列可以引用)
	Column func(name string) (typ string, ok bool)            // 这个操作的表的列(nil = 不能引用列: insert / 键)
	Ref    func(op, col string) (typ string, err error)       // 前面的操作的返回值
	Input  func(name string) (typ string, err error)          // 请求字段的 SQL 类型(枚举 = "enum")
	Enum   func(table, col, name string) (db string, ok bool) // 枚举列的值名 → 数据库值
	EnumOf func(table, col string) bool                       // 这一列是枚举
	// R4: 每项的元素的字段(nil = 没有 每项);操作名.列 是枚举的列时 → 表(和枚举的值名比较);信封.id 能不能用
	Item    func(name string) (typ string, err error)
	RefEnum func(op, col string) (table string, ok bool)
	Env     bool
}

// Resolve: 给树加上类型(Type),名字 → 列 / 枚举的值;want = 这个值要写进的列的类型("" = 条件)。
// 检查: 加减只用于整数 / 十进制;比较的两边可以比较(文本、布尔、枚举只能 = / <>);枚举的值名只能和枚举的列比较或写进枚举的列;
// 整数的输入不能比对方的列宽(int8 的输入 + int4 的列 → 数据库按 int4 收参数,溢出)。
func Resolve(x *ir.CmdExpr, sc Scope, want, wantTable, wantCol string) error {
	if want == "" { // 条件
		return resolveCond(x, sc)
	}
	t, err := resolveValue(x, sc, want, wantTable, wantCol)
	if err != nil {
		return err
	}
	if !assignable(t, want) {
		return fmt.Errorf("值的类型 %s 不能写进 %s 的列", t, want)
	}
	return nil
}

func resolveCond(x *ir.CmdExpr, sc Scope) error {
	switch {
	case x.Op == "and":
		for _, a := range x.Args {
			if err := resolveCond(a, sc); err != nil {
				return err
			}
		}
		return nil
	case cmpWords[x.Op]:
		a, b := x.Args[0], x.Args[1]
		// 先解释有类型的一边(列 / 引用 / 输入),常量和枚举的值名跟着它
		ta, ea := typeHint(a, sc)
		tb, eb := typeHint(b, sc)
		if ea != nil {
			return ea
		}
		if eb != nil {
			return eb
		}
		hintT, hintTable, hintCol := ta, enumTableOf(a, sc), enumColOf(a, sc)
		if hintT == "" {
			hintT, hintTable, hintCol = tb, enumTableOf(b, sc), enumColOf(b, sc)
		}
		for _, side := range []*ir.CmdExpr{a, b} { // 操作名.列 是枚举的列: 另一边的值名按这一列的枚举解释
			if side.Op == "ref" && sc.RefEnum != nil {
				if t, ok := sc.RefEnum(side.Ref, side.Name); ok {
					hintT, hintTable, hintCol = "enum:"+t+"."+side.Name, t, side.Name
				}
			}
		}
		if hintT == "" {
			return fmt.Errorf("比较的两边至少要有一个列 / 输入 / 前面操作的值")
		}
		ra, err := resolveValue(a, sc, hintT, hintTable, hintCol)
		if err != nil {
			return err
		}
		rb, err := resolveValue(b, sc, hintT, hintTable, hintCol)
		if err != nil {
			return err
		}
		if !comparable(ra, rb) {
			return fmt.Errorf("%s 和 %s 不能比较", ra, rb)
		}
		if (ra == "text" || ra == "bool" || isEnumT(ra)) && x.Op != "=" && x.Op != "<>" {
			return fmt.Errorf("%s 只能用 = / <> 比较(文本的大小顺序取决于数据库的排序规则)", ra)
		}
		if !mentionsRow(x) && sc.Column != nil {
			return fmt.Errorf("条件要用到这一行的列(只有输入 / 常量的条件不是条件更新)")
		}
		return nil
	}
	return fmt.Errorf("条件要写成比较(= <> < <= > >=),多个用 并且 连接")
}

// typeHint: 不依赖另一边就能决定的类型(常量 / 枚举的值名 → "")。
func typeHint(x *ir.CmdExpr, sc Scope) (string, error) {
	switch x.Op {
	case "name":
		if sc.Column != nil {
			if t, ok := sc.Column(x.Name); ok {
				if sc.EnumOf != nil && sc.EnumOf(sc.Table, x.Name) {
					return "enum:" + sc.Table + "." + x.Name, nil
				}
				return t, nil
			}
		}
		return "", nil
	case "ref":
		return sc.Ref(x.Ref, x.Name)
	case "input":
		t, err := sc.Input(x.Name)
		if t == "enum" {
			return "", err // 枚举的输入: 跟着比较的列
		}
		return t, err
	case "item":
		if sc.Item == nil {
			return "", fmt.Errorf("项.%s: 没有 每项(项. 只能用在 每项 的操作里)", x.Name)
		}
		return sc.Item(x.Name)
	case "env":
		return "text", nil
	case "now":
		return "timestamptz", nil
	case "+", "-":
		ta, err := typeHint(x.Args[0], sc)
		if err != nil || ta != "" {
			return ta, err
		}
		return typeHint(x.Args[1], sc)
	}
	return "", nil
}

func enumTableOf(x *ir.CmdExpr, sc Scope) string {
	if x.Op == "name" && sc.EnumOf != nil && sc.EnumOf(sc.Table, x.Name) {
		return sc.Table
	}
	return ""
}

func enumColOf(x *ir.CmdExpr, sc Scope) string {
	if enumTableOf(x, sc) != "" {
		return x.Name
	}
	return ""
}

// resolveValue: 一个值的类型。want = 跟着的类型(常量 / 枚举的值名 / 枚举的输入用);枚举的列的类型写成 enum:表.列。
func resolveValue(x *ir.CmdExpr, sc Scope, want, wantTable, wantCol string) (string, error) {
	base := want
	if isEnumT(want) {
		base = "int4"
	}
	switch x.Op {
	case "name":
		if sc.Column != nil {
			if t, ok := sc.Column(x.Name); ok {
				x.Op, x.Type = "col", t
				if sc.EnumOf != nil && sc.EnumOf(sc.Table, x.Name) {
					return "enum:" + sc.Table + "." + x.Name, nil
				}
				return t, nil
			}
		}
		if wantTable != "" && sc.Enum != nil {
			if db, ok := sc.Enum(wantTable, wantCol, x.Name); ok {
				x.Op, x.Value, x.Type = "const", db, base
				return want, nil
			}
			return "", fmt.Errorf("%s 不是枚举 %s.%s 的值名", x.Name, wantTable, wantCol)
		}
		if sc.Column == nil {
			return "", fmt.Errorf("%s: 这里不能引用列(新建的值 / 键 用 输入.x、前面的操作的 操作名.列、常量、现在)", x.Name)
		}
		return "", fmt.Errorf("%s 不是表 %s 的列", x.Name, sc.Table)
	case "ref":
		t, err := sc.Ref(x.Ref, x.Name)
		if err != nil {
			return "", err
		}
		x.Type = t
		if isEnumT(want) && sc.RefEnum != nil {
			if et, ok := sc.RefEnum(x.Ref, x.Name); ok && want == "enum:"+et+"."+x.Name {
				return want, nil // 同一个枚举的列(数据库值)
			}
		}
		return t, nil
	case "item":
		if sc.Item == nil {
			return "", fmt.Errorf("项.%s: 没有 每项(项. 只能用在 每项 的操作里)", x.Name)
		}
		t, err := sc.Item(x.Name)
		if err != nil {
			return "", err
		}
		if isInt(t) && isInt(base) && intWidth(t) > intWidth(base) {
			return "", fmt.Errorf("项.%s 是 %s,比对方(%s)宽: 数据库按 %s 收参数,大的值会溢出", x.Name, t, base, base)
		}
		x.Type = t
		return t, nil
	case "env":
		if !sc.Env {
			return "", fmt.Errorf("信封.id 只能用在消息处理器里")
		}
		x.Type = "text"
		return "text", nil
	case "null":
		if want == "" {
			return "", fmt.Errorf("空 只能作为值(写进列)")
		}
		x.Type = base
		return want, nil
	case "if":
		s2 := sc
		s2.Column = nil // 条件只看 输入 / 项 / 常量(在 Go 里算好,不进 SQL)
		if err := resolveCond(x.Args[0], s2); err != nil {
			return "", fmt.Errorf("如果 的条件: %w", err)
		}
		if mentionsRow(x.Args[0]) || len(Refs(x.Args[0])) > 0 {
			return "", fmt.Errorf("如果 的条件只能用 输入 / 消息 / 项 / 常量")
		}
		for _, br := range x.Args[1:] {
			if br.Op != "const" && br.Op != "name" && br.Op != "null" && br.Op != "input" && br.Op != "item" {
				return "", fmt.Errorf("如果 的分支写 常量 / 枚举的值名 / 空 / 输入 / 项")
			}
			t, err := resolveValue(br, s2, want, wantTable, wantCol)
			if err != nil {
				return "", err
			}
			if !assignable(t, want) {
				return "", fmt.Errorf("如果 的分支 %s 不能写进 %s 的列", t, want)
			}
		}
		x.Type = base
		return want, nil
	case "input":
		t, err := sc.Input(x.Name)
		if err != nil {
			return "", err
		}
		if t == "enum" {
			if wantTable == "" {
				return "", fmt.Errorf("输入.%s 是枚举: 只能写进枚举的列或者和枚举的列比较", x.Name)
			}
			x.Type, x.Table, x.Column = "int4", wantTable, wantCol
			return want, nil
		}
		if isInt(t) && isInt(base) && intWidth(t) > intWidth(base) {
			return "", fmt.Errorf("输入.%s 是 %s,比对方(%s)宽: 数据库按 %s 收参数,大的值会溢出(把输入改成 整数32)", x.Name, t, base, base)
		}
		x.Type = t
		return t, nil
	case "const":
		if x.Type == "text" {
			return "text", nil
		}
		switch {
		case isEnumT(want) && sc.Enum != nil: // 设计稿
			return "", fmt.Errorf("枚举的列写值名(不是数字 %s)", x.Value)
		case isEnumT(want) && !strings.Contains(x.Value, "."): // spec: 枚举已经是数据库值
			x.Type = "int4"
			return want, nil
		case isInt(base) && !strings.Contains(x.Value, "."):
			x.Type = base
			return base, nil
		case base == "numeric":
			x.Type = "numeric"
			return "numeric", nil
		case base == "bool" && (x.Value == "true" || x.Value == "false"):
			x.Type = "bool"
			return "bool", nil
		}
		return "", fmt.Errorf("常量 %s 和 %s 的类型不一致", x.Value, want)
	case "now":
		x.Type = "timestamptz"
		return "timestamptz", nil
	case "+", "-":
		ta, err := resolveValue(x.Args[0], sc, want, "", "")
		if err != nil {
			return "", err
		}
		tb, err := resolveValue(x.Args[1], sc, want, "", "")
		if err != nil {
			return "", err
		}
		if !isNum(ta) || !isNum(tb) {
			return "", fmt.Errorf("加减只用于整数 / 十进制(%s %s %s)", ta, x.Op, tb)
		}
		t := widest(ta, tb)
		if isInt(t) && isNum(base) && base == "numeric" {
			t = widest(t, "int8")
		}
		x.Type = t
		return t, nil
	}
	return "", fmt.Errorf("这里不能写 %s", x.Op)
}

func isEnumT(t string) bool { return strings.HasPrefix(t, "enum:") }
func isInt(t string) bool   { return t == "int2" || t == "int4" || t == "int8" }
func isNum(t string) bool   { return isInt(t) || t == "numeric" }

func intWidth(t string) int {
	return map[string]int{"int2": 2, "int4": 4, "int8": 8}[t]
}

func widest(a, b string) string {
	if a == "numeric" || b == "numeric" {
		return "numeric"
	}
	if intWidth(a) >= intWidth(b) {
		return a
	}
	return b
}

func comparable(a, b string) bool {
	if isNum(a) && isNum(b) {
		return true
	}
	return a == b
}

// assignable: 值的类型 t 能写进 want 的列。
func assignable(t, want string) bool {
	switch {
	case t == want:
		return true
	case isEnumT(want):
		return t == want
	case isInt(want) && isInt(t):
		return intWidth(t) <= intWidth(want)
	case want == "numeric":
		return isNum(t)
	case want == "timestamptz" || want == "timestamp":
		return t == "timestamptz"
	}
	return false
}

func mentionsRow(x *ir.CmdExpr) bool {
	if x.Op == "if" { // 如果 的条件不看这一行(分支的值名不是列)
		return false
	}
	if x.Op == "col" || x.Op == "name" {
		return true
	}
	for _, a := range x.Args {
		if mentionsRow(a) {
			return true
		}
	}
	return false
}

// Format: 树 → 文本(spec 的写法;枚举的值已换成数据库值,现在 → now())。
func Format(x *ir.CmdExpr) string {
	switch x.Op {
	case "input":
		return InputPrefix + x.Name
	case "item":
		return ItemPrefix + x.Name
	case "env":
		return EnvPrefix + x.Name
	case "null":
		return "空"
	case "if":
		return "如果 " + Format(x.Args[0]) + " 则 " + Format(x.Args[1]) + " 否则 " + Format(x.Args[2])
	case "ref":
		return x.Ref + "." + x.Name
	case "col", "name":
		return x.Name
	case "now":
		return "now()"
	case "const":
		if x.Type == "text" {
			return "'" + strings.ReplaceAll(x.Value, "'", "''") + "'"
		}
		return x.Value
	case "and":
		parts := make([]string, len(x.Args))
		for i, a := range x.Args {
			parts[i] = Format(a)
		}
		return strings.Join(parts, " 并且 ")
	}
	if len(x.Args) == 2 {
		return Format(x.Args[0]) + " " + x.Op + " " + Format(x.Args[1])
	}
	return "?"
}

// InputNames / Refs / Cols: 表达式里用到的请求字段 / 前面的操作的返回值 / 这一行的列。
func InputNames(x *ir.CmdExpr) []string { return collect(x, "input") }
func Cols(x *ir.CmdExpr) []string       { return collect(x, "col") }

// Refs: 用到的 操作名.列。
func Refs(x *ir.CmdExpr) [][2]string {
	var out [][2]string
	walk(x, func(e *ir.CmdExpr) {
		if e.Op == "ref" {
			out = append(out, [2]string{e.Ref, e.Name})
		}
	})
	return out
}

func collect(x *ir.CmdExpr, op string) []string {
	var out []string
	walk(x, func(e *ir.CmdExpr) {
		if e.Op == op {
			out = append(out, e.Name)
		}
	})
	return out
}

func walk(x *ir.CmdExpr, f func(*ir.CmdExpr)) {
	if x == nil {
		return
	}
	f(x)
	for _, a := range x.Args {
		walk(a, f)
	}
}
