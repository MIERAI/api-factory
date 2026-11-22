package design

// 派生参数: 筛选的值由 现在 和输入计算,不出现在请求里(设计稿 docs/design/r2-relations.md §2.5)。
//
//	派生参数:
//	  stuck_before: { 计算: 现在 - stuck_minutes 分钟, 筛选: updated_at 小于, 说明: … }
//	  month_start:  { 计算: 月初(base_date - 1 个月), 筛选: sales_date 大于等于 }
//
// 计算的写法(从左到右):
//
//	值   = 项 { (+ | -) 个数 单位 }
//	项   = 现在 | 今天 | 输入名 | 月初(值) | 日初(值) | (值)
//	个数 = 整数 | 输入名(整数的输入)
//	单位 = 分钟 | 小时 | 天 | 个月(月)
//
// 现在 = 请求开始的时刻(服务器的时钟;测试时由例子的 现在 固定)。今天 = 现在 在领域的时区里的日期(日期,
// 和日期的列 / 整数编码的日期的列比较)。月初 / 日初 = 截到那个月 / 那一天的开始,
// 时间先换算到领域的时区。月的加减按 Postgres(1/31 + 1 个月 = 2/28)。译成核心代数: now / add / trunc / local_date。

import (
	"fmt"
	"strings"
	"unicode"

	"apifactory/factory/core"
	"apifactory/factory/kinds"
	"apifactory/factory/spec"
)

type Derived struct {
	Name   string
	Expr   string // 计算
	Filter string // "updated_at 小于"
	Desc   string
	Line   int
}

var derivedUnits = map[string]string{"分钟": core.Minute, "小时": core.Hour, "天": core.Day, "个月": core.Month, "月": core.Month}

// DerivedKeys: 派生参数可以写的键。
func DerivedKeys() []string { return []string{"计算", "筛选", "说明"} }

type exprParser struct {
	src     []rune
	pos     int
	input   func(name string) (core.Kind, bool) // 输入名 → 核心类型
	used    map[string]core.Kind
	usesNow bool
}

func (p *exprParser) space() {
	for p.pos < len(p.src) && unicode.IsSpace(p.src[p.pos]) {
		p.pos++
	}
}

func (p *exprParser) word() string {
	p.space()
	start := p.pos
	for p.pos < len(p.src) && (unicode.IsLetter(p.src[p.pos]) || unicode.IsDigit(p.src[p.pos]) || p.src[p.pos] == '_') {
		// 中文的单位紧跟在数字后面也能分开(5分钟): 数字和汉字是两个词
		if p.pos > start && isDigit(p.src[start]) != isDigit(p.src[p.pos]) {
			break
		}
		if p.pos > start && isHan(p.src[start]) != isHan(p.src[p.pos]) {
			break
		}
		p.pos++
	}
	return string(p.src[start:p.pos])
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }
func isHan(r rune) bool   { return unicode.Is(unicode.Han, r) }

func (p *exprParser) peek(r rune) bool {
	p.space()
	return p.pos < len(p.src) && p.src[p.pos] == r
}

func (p *exprParser) value() (*core.Expr, error) {
	x, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		var neg bool
		switch {
		case p.peek('+'):
		case p.peek('-'):
			neg = true
		default:
			return x, nil
		}
		p.pos++
		w := p.word()
		var n *core.Expr
		switch {
		case w == "":
			return nil, fmt.Errorf("+ / - 之后要写个数(整数或整数的输入)")
		case strings.Trim(w, "0123456789") == "":
			var v int64
			fmt.Sscan(w, &v)
			n = core.Const(v, core.Type{Kind: core.Int32})
		default:
			k, ok := p.input(w)
			if !ok {
				return nil, fmt.Errorf("%s 不是输入", w)
			}
			if k != core.Int32 && k != core.Int64 {
				return nil, fmt.Errorf("个数 %s 必须是整数的输入", w)
			}
			p.used[w] = k
			n = core.Param(w)
		}
		u := p.word()
		unit, ok := derivedUnits[u]
		if !ok {
			return nil, fmt.Errorf("单位 %q 不对(分钟 / 小时 / 天 / 个月)", u)
		}
		x = core.Add(x, n, unit, neg, "")
	}
}

func (p *exprParser) term() (*core.Expr, error) {
	if p.peek('(') {
		p.pos++
		x, err := p.value()
		if err != nil {
			return nil, err
		}
		if !p.peek(')') {
			return nil, fmt.Errorf("缺少 )")
		}
		p.pos++
		return x, nil
	}
	w := p.word()
	switch w {
	case "":
		return nil, fmt.Errorf("第 %d 个字符: 要写 现在 / 今天 / 输入名 / 月初(…) / 日初(…)", p.pos+1)
	case "现在":
		p.usesNow = true
		return core.Now(), nil
	case "今天": // 时区由编译器另外设置(core.SetZone: 领域的时区)
		p.usesNow = true
		return core.Today(""), nil
	case "月初", "日初":
		if !p.peek('(') {
			return nil, fmt.Errorf("%s 后面要写 (…)", w)
		}
		p.pos++
		x, err := p.value()
		if err != nil {
			return nil, err
		}
		if !p.peek(')') {
			return nil, fmt.Errorf("%s(…: 缺少 )", w)
		}
		p.pos++
		unit := core.Month
		if w == "日初" {
			unit = core.Day
		}
		return core.Trunc(x, unit, ""), nil
	}
	k, ok := p.input(w)
	if !ok {
		return nil, fmt.Errorf("%s 不是输入(可以写 现在 / 今天 / 输入名 / 月初(…) / 日初(…))", w)
	}
	if k != core.Date && k != core.Timestamp {
		return nil, fmt.Errorf("输入 %s 不是日期 / 时间(只能当个数用)", w)
	}
	p.used[w] = k
	return core.Param(w), nil
}

// parseDerived: 计算的写法 → 核心代数的表达式 + 用到的输入 + 有没有用到 现在。
func parseDerived(src string, input func(string) (core.Kind, bool)) (*core.Expr, map[string]core.Kind, bool, error) {
	p := &exprParser{src: []rune(src), input: input, used: map[string]core.Kind{}}
	x, err := p.value()
	if err != nil {
		return nil, nil, false, err
	}
	p.space()
	if p.pos != len(p.src) {
		return nil, nil, false, fmt.Errorf("第 %d 个字符之后有多余的内容 %q", p.pos, string(p.src[p.pos:]))
	}
	return x, p.used, p.usesNow, nil
}

// compileDerived: 设计稿的 派生参数 → spec 的 datasource.derived。返回有没有用到 现在。
func (c *ctx) compileDerived(a *API, table string, e *errs) (map[string]spec.Derived, bool) {
	if len(a.Derived) == 0 {
		return nil, false
	}
	inputs := map[string]*Input{}
	for _, in := range a.Inputs {
		inputs[in.Name] = in
	}
	out := map[string]spec.Derived{}
	usesNow := false
	for _, d := range a.Derived {
		if _, dup := inputs[d.Name]; dup {
			e.add(d.Line, "派生参数 %s: 和输入重名(派生参数不出现在请求里)", d.Name)
			continue
		}
		x, used, now, err := parseDerived(d.Expr, func(name string) (core.Kind, bool) {
			in, ok := inputs[name]
			if !ok {
				return "", false
			}
			if in.Encoding == EncodingYMD { // 整数编码的日期: 在计算里是日期(ymd(参数))
				return core.Date, true
			}
			k, ok := kinds.ByName(in.Type)
			if !ok || k.Core == "" {
				return "", false
			}
			return k.Core, true
		})
		if err != nil {
			e.add(d.Line, "派生参数 %s: 计算 %q: %v", d.Name, d.Expr, err)
			continue
		}
		x = wrapYMDParams(x, func(name string) bool { in := inputs[name]; return in != nil && in.Encoding == EncodingYMD })
		usesNow = usesNow || now
		ins := map[string]string{}
		for n, k := range used {
			in := inputs[n]
			if in.Filter != "" {
				e.add(in.Line, "输入 %s: 用在派生参数 %s 里的输入不能再写 筛选(要筛选两次的话,另写一个派生参数)", n, d.Name)
			}
			if in.Encoding == EncodingYMD { // 请求里是整数(参数的类型),计算里按 ymd 读成日期
				if t, ok := kinds.ByName(in.Type); ok && t.Core != "" {
					k = t.Core
				}
			}
			ins[n] = string(k)
		}
		parts := strings.Fields(d.Filter)
		op, ok := "", false
		if len(parts) == 2 {
			op, ok = c.lib.FilterID(parts[1])
		}
		if !ok {
			e.add(d.Line, "派生参数 %s: 筛选 %q 无法识别。写成 \"列 <筛选单元>\"(等于 / 小于 / 大于等于 …)", d.Name, d.Filter)
			continue
		}
		switch op {
		case "eq", "ne", "gt", "gte", "lt", "lte":
		default:
			e.add(d.Line, "派生参数 %s: 筛选只能是比较(等于 / 不等于 / 大于 / 大于等于 / 小于 / 小于等于)", d.Name)
			continue
		}
		col := parts[0]
		cc, has := c.column(table, col)
		if !has {
			e.add(d.Line, "派生参数 %s: 筛选的列 %s 不在表 %s 里", d.Name, col, table)
			continue
		}
		if k, ok := kinds.BySQL(cc.Type); ok && (k.Core == core.Int32 || k.Core == core.Int64) {
			if t := c.d.Tables[table]; t == nil || t.ColEncode[col] != EncodingYMD {
				e.add(d.Line, "派生参数 %s: 列 %s 是整数。整数里存的是日期(YYYYMMDD)的话,在领域的列上写 编码: %s", d.Name, col, EncodingYMD)
				continue
			}
		}
		if xt, err := (&core.Pipeline{Params: derivedParams(ins)}).TypeOf(x, nil); err == nil {
			if k, ok := kinds.BySQL(cc.Type); ok && k.Core == core.Timestamp && xt.Kind == core.Date {
				e.add(d.Line, "派生参数 %s: 计算的结果是日期(今天 / 日期的输入),列 %s 是时间: 时间的列请和 日初(现在) 这类时间比较", d.Name, col)
				continue
			}
		}
		text, err := core.Format(x)
		if err != nil {
			e.add(d.Line, "派生参数 %s: %v", d.Name, err)
			continue
		}
		out[d.Name] = spec.Derived{Expr: text, Column: col, Op: op, Inputs: ins, TimeZone: c.d.TZ}
	}
	return out, usesNow
}

// derivedParams: 计算里用到的输入的声明(请求里的类型: 整数编码的日期是整数,计算里是 ymd(参数)),算计算的结果的类型用。
func derivedParams(ins map[string]string) []core.ParamDecl {
	var out []core.ParamDecl
	for n, k := range ins {
		out = append(out, core.ParamDecl{Name: n, Type: core.Type{Kind: core.Kind(k)}})
	}
	return out
}

// wrapYMDParams: 整数编码的日期的输入 → ymd(参数)(计算里当作日期)。
func wrapYMDParams(x *core.Expr, encoded func(string) bool) *core.Expr {
	if x == nil {
		return nil
	}
	if x.Op == "param" && encoded(x.Name) {
		return core.YMD(x)
	}
	c := *x
	c.Args = nil
	for _, a := range x.Args {
		c.Args = append(c.Args, wrapYMDParams(a, encoded))
	}
	return &c
}
