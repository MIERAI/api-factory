// Package where: 设计稿的「固定条件」(不来自请求的条件)→ 核心代数的表达式。
//
// 写法(词之间用空格隔开):
//
//	条件   = 并且式 { "或者" 并且式 }
//	并且式 = 单项 { "并且" 单项 }
//	单项   = "不是" 单项 | "(" 条件 ")" | 值 比较 值 | 列 "为空" | 列 "不为空"
//	比较   = "=" | "<>" | "<" | "<=" | ">" | ">="
//	值     = 列名 | 数字 | '文本' (日期也写成 '2026-01-01')
//
// 例: price_in_tax <> old_price_in_tax 或者 biz_deal_id = 99999999
//
// 空值: 比较的任一边为空 → 不确定(三值逻辑),这一行不留下。要留下空值的行,写 "或者 列 为空"。
// 两边不能都是常量;常量的类型跟着另一边的列。
package where

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"

	"apifactory/factory/core"
)

// Parse: 一个固定条件 → 表达式。colType: 列名 → 类型(不是列 → false)。
func Parse(src string, colType func(string) (core.Type, bool)) (*core.Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, colType: colType}
	e, err := p.or()
	if err != nil {
		return nil, fmt.Errorf("固定条件 %q: %w", src, err)
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("固定条件 %q: %q 之后有多余的内容", src, p.toks[p.pos-1])
	}
	return e, nil
}

// Columns: 条件里用到的列(设计稿的检查用)。
func Columns(e *core.Expr) []string {
	var out []string
	var walk func(x *core.Expr)
	walk = func(x *core.Expr) {
		if x == nil {
			return
		}
		if x.Op == "col" {
			out = append(out, x.Name)
		}
		for _, a := range x.Args {
			walk(a)
		}
	}
	walk(e)
	return out
}

var cmpWords = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

func lex(src string) ([]string, error) {
	var out []string
	rs := []rune(src)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(' || r == ')' || r == '（' || r == '）':
			out = append(out, map[rune]string{'(': "(", ')': ")", '（': "(", '）': ")"}[r])
			i++
		case r == '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			if j == len(rs) {
				return nil, fmt.Errorf("固定条件 %q: 文本的引号没有关闭", src)
			}
			out = append(out, string(rs[i:j+1]))
			i = j + 1
		case strings.ContainsRune("=<>", r):
			j := i
			for j < len(rs) && strings.ContainsRune("=<>", rs[j]) {
				j++
			}
			op := string(rs[i:j])
			if !cmpWords[op] {
				return nil, fmt.Errorf("固定条件 %q: 比较 %q 无法识别(= <> < <= > >=)", src, op)
			}
			out = append(out, op)
			i = j
		default:
			j := i
			for j < len(rs) && !unicode.IsSpace(rs[j]) && !strings.ContainsRune("()（）'=<>", rs[j]) {
				j++
			}
			out = append(out, string(rs[i:j]))
			i = j
		}
	}
	return out, nil
}

type parser struct {
	toks    []string
	pos     int
	colType func(string) (core.Type, bool)
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *parser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *parser) or() (*core.Expr, error) {
	return p.chain("或者", p.and, core.Or)
}

func (p *parser) and() (*core.Expr, error) {
	return p.chain("并且", p.unary, core.And)
}

func (p *parser) chain(word string, sub func() (*core.Expr, error), join func(...*core.Expr) *core.Expr) (*core.Expr, error) {
	first, err := sub()
	if err != nil {
		return nil, err
	}
	xs := []*core.Expr{first}
	for p.peek() == word {
		p.pos++
		x, err := sub()
		if err != nil {
			return nil, err
		}
		xs = append(xs, x)
	}
	if len(xs) == 1 {
		return first, nil
	}
	return join(xs...), nil
}

func (p *parser) unary() (*core.Expr, error) {
	switch p.peek() {
	case "不是":
		p.pos++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return core.Not(x), nil
	case "(":
		p.pos++
		x, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, fmt.Errorf("括号没有关闭")
		}
		return x, nil
	case "":
		return nil, fmt.Errorf("条件不完整")
	}
	left := p.next()
	switch p.peek() {
	case "为空", "不为空":
		t, ok := p.colType(left)
		if !ok {
			return nil, fmt.Errorf("%s 不是列(为空 / 不为空 只能用于列)", left)
		}
		_ = t
		x := core.IsNull(core.Col(left))
		if p.next() == "不为空" {
			x = core.Not(x)
		}
		return x, nil
	}
	op := p.next()
	if !cmpWords[op] {
		return nil, fmt.Errorf("%s 之后需要 比较(= <> < <= > >=)或 为空 / 不为空,现在是 %q", left, op)
	}
	right := p.next()
	if right == "" {
		return nil, fmt.Errorf("%s %s 之后缺少值", left, op)
	}
	lt, lcol := p.colType(left)
	rt, rcol := p.colType(right)
	switch {
	case lcol && rcol:
		return core.Cmp(op, core.Col(left), core.Col(right)), nil
	case lcol:
		c, err := constant(right, lt)
		if err != nil {
			return nil, err
		}
		return core.Cmp(op, core.Col(left), c), nil
	case rcol:
		c, err := constant(left, rt)
		if err != nil {
			return nil, err
		}
		return core.Cmp(op, c, core.Col(right)), nil
	}
	return nil, fmt.Errorf("%s %s %s: 至少一边要是列(列名写错了?)", left, op, right)
}

// constant: 常量的写法 → 和列同类型的常量。
func constant(s string, t core.Type) (*core.Expr, error) {
	typ := core.Type{Kind: t.Kind}
	quoted := len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\''
	body := strings.Trim(s, "'")
	switch t.Kind {
	case core.Int32, core.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || quoted {
			return nil, fmt.Errorf("%s 不是整数(列是整数)", s)
		}
		return core.Const(n, typ), nil
	case core.Decimal:
		r, ok := new(big.Rat).SetString(s)
		if !ok || quoted {
			return nil, fmt.Errorf("%s 不是数字(列是小数)", s)
		}
		return core.Const(r, typ), nil
	case core.Text:
		if !quoted {
			return nil, fmt.Errorf("%s: 文本的常量要用单引号括起来('%s');如果是列名,这张表里没有这一列", s, s)
		}
		return core.Const(body, typ), nil
	case core.Date:
		d, err := time.Parse("2006-01-02", body)
		if err != nil || !quoted {
			return nil, fmt.Errorf("%s: 日期写成 '2026-01-01'", s)
		}
		return core.Const(d, typ), nil
	case core.Bool:
		switch s {
		case "真":
			return core.Const(true, typ), nil
		case "假":
			return core.Const(false, typ), nil
		}
		return nil, fmt.Errorf("%s: 布尔写成 真 / 假", s)
	}
	return nil, fmt.Errorf("%s: %s 类型的列还不能和常量比较", s, t.Kind)
}
