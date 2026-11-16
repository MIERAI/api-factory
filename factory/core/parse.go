package core

// 单元定义的表达式语法(单元库是数据,不是代码):
//
//	expr  = name "(" [arg {"," arg}] ")" | ident
//	arg   = expr | 运算符(= <> < <= > >=)
//
// ident 由调用方绑定(单元的参数: 列 → col(x),值 → param(y))。
// 元件名: cmp and or not is_null absent in_list starts_with。

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
	name := p.token()
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
	case "in_list", "starts_with":
		if err := need(2); err != nil {
			return nil, err
		}
		n, err := paramName(1)
		if err != nil {
			return nil, err
		}
		return &Expr{Op: name, Name: n, Args: args[:1]}, nil
	}
	return nil, fmt.Errorf("不认识的元件 %s(单元只能由基本元件组成)", name)
}
