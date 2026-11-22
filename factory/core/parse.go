package core

// 单元定义的表达式语法(单元库是数据,不是代码):
//
//	expr  = name "(" [arg {"," arg}] ")" | ident
//	arg   = expr | 运算符(= <> < <= > >=)
//
// ident 由调用方绑定(单元的参数: 列 → col(x),值 → param(y))。
// 元件名: cmp and or not is_null absent in_list starts_with contains now add trunc ymd local_date。
// 时间运算(派生参数的值): now()、add(x, n, 单位)、trunc(x, 单位);n = 整数常量(可以带 -)或名字(带 - = 减),
// 单位 = minute / hour / day / month(写在单位的位置上,不经过 bind)。时区由调用方另外设置(SetZone)。

import (
	"fmt"
	"strings"
	"unicode"
)

type parser struct {
	src  []rune
	pos  int
	bind func(string) (*Expr, bool)
}

// ParseExpr: 解析单元的定义。bind 把单元参数名换成表达式。
func ParseExpr(src string, bind func(string) (*Expr, bool)) (*Expr, error) {
	p := &parser{src: []rune(src), bind: bind}
	e, err := p.expr()
	if err != nil {
		return nil, fmt.Errorf("%q: %w", src, err)
	}
	p.space()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("%q: 第 %d 个字符之后有多余的内容", src, p.pos)
	}
	return e, nil
}

func (p *parser) space() {
	for p.pos < len(p.src) && unicode.IsSpace(p.src[p.pos]) {
		p.pos++
	}
}

func (p *parser) token() string {
	p.space()
	start := p.pos
	if p.pos < len(p.src) && strings.ContainsRune("=<>", p.src[p.pos]) {
		for p.pos < len(p.src) && strings.ContainsRune("=<>", p.src[p.pos]) {
			p.pos++
		}
		return string(p.src[start:p.pos])
	}
	for p.pos < len(p.src) && (unicode.IsLetter(p.src[p.pos]) || unicode.IsDigit(p.src[p.pos]) || p.src[p.pos] == '_') {
		p.pos++
	}
	return string(p.src[start:p.pos])
}

func (p *parser) peek(r rune) bool {
	p.space()
	return p.pos < len(p.src) && p.src[p.pos] == r
}

// arg: 表达式或运算符(只在 cmp 的第一个参数)
func (p *parser) arg() (string, *Expr, error) {
	save := p.pos
	t := p.token()
	if cmpOps[t] {
		return t, nil, nil
	}
	p.pos = save
	e, err := p.expr()
	return "", e, err
}

func (p *parser) expr() (*Expr, error) {
	if p.peek('-') { // 负号: 整数常量 → 负数;其他 → add 的"减"(只能用在 add 的个数上)
		p.pos++
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		if x.Op == "const" {
			if v, ok := x.Value.(int64); ok {
				return Const(-v, Type{Kind: Int32}), nil
			}
		}
		return &Expr{Op: "neg", Args: []*Expr{x}}, nil
	}
	name := p.token()
	if name != "" && strings.Trim(name, "0123456789") == "" { // 整数常量
		var v int64
		if _, err := fmt.Sscan(name, &v); err != nil {
			return nil, fmt.Errorf("整数 %s: %v", name, err)
		}
		return Const(v, Type{Kind: Int32}), nil
	}
	if timeUnits[name] && !p.peek('(') {
		return &Expr{Op: "unit", Name: name}, nil
	}
	if name == "" {
		return nil, fmt.Errorf("第 %d 个字符: 需要名字", p.pos)
	}
	if !p.peek('(') {
		e, ok := p.bind(name)
		if !ok {
			return nil, fmt.Errorf("不认识的名字 %s", name)
		}
		return e, nil
	}
	p.pos++ // (
	var ops []string
	var args []*Expr
	for !p.peek(')') {
		op, e, err := p.arg()
		if err != nil {
			return nil, err
		}
		if op != "" {
			ops = append(ops, op)
		} else {
			args = append(args, e)
		}
		if p.peek(',') {
			p.pos++
		} else if !p.peek(')') {
			return nil, fmt.Errorf("第 %d 个字符: 需要 , 或 )", p.pos)
		}
	}
	p.pos++ // )
	paramName := func(i int) (string, error) {
		if i >= len(args) || args[i].Op != "param" {
			return "", fmt.Errorf("%s 的第 %d 个参数必须是请求参数", name, i+1)
		}
		return args[i].Name, nil
	}
	need := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s 需要 %d 个参数,实际 %d 个", name, n, len(args))
		}
		return nil
	}
	switch name {
	case "cmp":
		if len(ops) != 1 {
			return nil, fmt.Errorf("cmp 的第一个参数必须是运算符")
		}
		if err := need(2); err != nil {
			return nil, err
		}
		return Cmp(ops[0], args[0], args[1]), nil
	case "and", "or":
		if len(args) < 2 {
			return nil, fmt.Errorf("%s 至少两个参数", name)
		}
		return &Expr{Op: name, Args: args}, nil
	case "not", "is_null":
		if err := need(1); err != nil {
			return nil, err
		}
		return &Expr{Op: name, Args: args}, nil
	case "absent":
		if err := need(1); err != nil {
			return nil, err
		}
		n, err := paramName(0)
		if err != nil {
			return nil, err
		}
		return Absent(n), nil
	case "in_tuples": // in_tuples(列组, 值组): 由调用方把 列组 / 值组 绑定成 tuple_cols / tuple_params
		if err := need(2); err != nil {
			return nil, err
		}
		if args[0].Op != "tuple_cols" || args[1].Op != "tuple_params" {
			return nil, fmt.Errorf("in_tuples 的参数必须是 列组 和 值组")
		}
		return &Expr{Op: "in_tuples", Args: args[0].Args, Params: args[1].Params}, nil
	case "in_list", "starts_with", "contains":
		if err := need(2); err != nil {
			return nil, err
		}
		n, err := paramName(1)
		if err != nil {
			return nil, err
		}
		return &Expr{Op: name, Name: n, Args: args[:1]}, nil
	case "now":
		if err := need(0); err != nil {
			return nil, err
		}
		return Now(), nil
	case "ymd":
		if err := need(1); err != nil {
			return nil, err
		}
		return YMD(args[0]), nil
	case "local_date": // 时区由调用方另外设置(SetZone)
		if err := need(1); err != nil {
			return nil, err
		}
		return LocalDate(args[0], ""), nil
	case "add":
		if err := need(3); err != nil {
			return nil, err
		}
		if args[2].Op != "unit" {
			return nil, fmt.Errorf("add 的第 3 个参数必须是单位(minute / hour / day / month)")
		}
		n, neg := args[1], false
		if n.Op == "neg" {
			n, neg = n.Args[0], true
		}
		return Add(args[0], n, args[2].Name, neg, ""), nil
	case "trunc":
		if err := need(2); err != nil {
			return nil, err
		}
		if args[1].Op != "unit" || (args[1].Name != Day && args[1].Name != Month) {
			return nil, fmt.Errorf("trunc 的第 2 个参数必须是单位(day / month)")
		}
		return Trunc(args[0], args[1].Name, ""), nil
	}
	return nil, fmt.Errorf("不认识的元件 %s(单元只能由基本元件组成)", name)
}

var timeUnits = map[string]bool{Minute: true, Hour: true, Day: true, Month: true}

// SetZone: 表达式里的 add / trunc / local_date 用领域的时区(时间的天 / 月按这个时区的日历)。
func SetZone(e *Expr, zone string) {
	if e == nil {
		return
	}
	if e.Op == "add" || e.Op == "trunc" || e.Op == "local_date" {
		e.Zone = zone
	}
	for _, a := range e.Args {
		SetZone(a, zone)
	}
}

// Format: 表达式 → ParseExpr 能读回的文本(派生参数写进式样书用)。只支持时间运算用到的元件: now / param / const / add / trunc / ymd。
func Format(e *Expr) (string, error) {
	switch e.Op {
	case "now":
		return "now()", nil
	case "param":
		return e.Name, nil
	case "const":
		if v, ok := e.Value.(int64); ok {
			return fmt.Sprint(v), nil
		}
	case "add":
		x, err := Format(e.Args[0])
		if err != nil {
			return "", err
		}
		n, err := Format(e.Args[1])
		if err != nil {
			return "", err
		}
		if e.Neg {
			n = "-" + n
		}
		return fmt.Sprintf("add(%s, %s, %s)", x, n, e.Name), nil
	case "trunc":
		x, err := Format(e.Args[0])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("trunc(%s, %s)", x, e.Name), nil
	case "ymd", "local_date":
		x, err := Format(e.Args[0])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s(%s)", e.Op, x), nil
	}
	return "", fmt.Errorf("format: 不支持 %s", e.Op)
}
