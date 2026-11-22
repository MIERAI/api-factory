package core

// 时间运算(派生参数的值;参考实现的日历: timeops.go):
//
//	add    Args[0](日期 / 时间)加 Args[1](整数)个单位,Name = 单位 ∈ minute hour day month;Neg = 减。
//	       月按 Postgres: 1 月 31 日 + 1 个月 = 2 月 28 日(29 日);日期只能加 day / month(结果还是日期)。
//	       时间的 day / month 按领域的时区(Zone)的日历加(minute / hour 是绝对时长)。任一边 NULL → NULL
//	trunc  Args[0] 截到 Name ∈ day month 的开始。时间先换算到领域的时区(Zone)再截,结果是那个时刻;
//	       日期的结果还是日期。NULL → NULL

import (
	"fmt"
	"time"
)

// 时间的单位。
const (
	Minute = "minute"
	Hour   = "hour"
	Day    = "day"
	Month  = "month"
)

// Add: x + n 个单位(neg: 减)。zone: 时间的 day / month 按哪个时区的日历。
func Add(x, n *Expr, unit string, neg bool, zone string) *Expr {
	return &Expr{Op: "add", Name: unit, Args: []*Expr{x, n}, Neg: neg, Zone: zone}
}

// Trunc: x 截到 unit 的开始(时间按 zone 的日历)。
func Trunc(x *Expr, unit, zone string) *Expr {
	return &Expr{Op: "trunc", Name: unit, Args: []*Expr{x}, Zone: zone}
}

func init() {
	registerExpr(exprPrim{
		op:     "add",
		typeOf: timeOpType,
		eval:   timeOpEval,
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			x, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			n, err := g.intArg(e.Args[1], lv)
			if err != nil {
				return "", err
			}
			t, _ := g.p.typeOf(e.Args[0], lv.in)
			sign := "+"
			if e.Neg {
				sign = "-"
			}
			field := map[string]string{Minute: "mins", Hour: "hours", Day: "days", Month: "months"}[e.Name]
			iv := fmt.Sprintf("make_interval(%s => %s)", field, n)
			switch {
			case t.Kind == Date && e.Name == Day: // date ± int → date
				return fmt.Sprintf("(%s %s %s)", x, sign, n), nil
			case t.Kind == Date: // date ± interval → timestamp(没有时区)→ 日期
				return fmt.Sprintf("((%s %s %s)::date)", x, sign, iv), nil
			case e.Name == Minute || e.Name == Hour: // 绝对时长(和时区无关)
				return fmt.Sprintf("(%s %s %s)", x, sign, iv), nil
			}
			// 天 / 月: 在领域的时区的日历上加(不受会话时区影响)
			z := quoteLit(zoneName(e.Zone))
			return fmt.Sprintf("(((%s AT TIME ZONE %s) %s %s) AT TIME ZONE %s)", x, z, sign, iv, z), nil
		},
	})
	registerExpr(exprPrim{
		op:     "trunc",
		typeOf: timeOpType,
		eval:   timeOpEval,
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			x, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			t, _ := g.p.typeOf(e.Args[0], lv.in)
			if t.Kind == Date { // timestamp(没有时区)上截: 不受会话时区影响
				return fmt.Sprintf("(date_trunc(%s, (%s)::timestamp)::date)", quoteLit(e.Name), x), nil
			}
			z := quoteLit(zoneName(e.Zone))
			return fmt.Sprintf("(date_trunc(%s, (%s AT TIME ZONE %s)) AT TIME ZONE %s)", quoteLit(e.Name), x, z, z), nil
		},
	})
}

// timeOpType: add / trunc 的类型(结果 = x 的类型;add 的个数可空 → 结果可空)。
func timeOpType(p *Pipeline, e *Expr, cur Schema) (Type, error) {
	want := 2
	units := map[string]bool{Minute: true, Hour: true, Day: true, Month: true}
	if e.Op == "trunc" {
		want, units = 1, map[string]bool{Day: true, Month: true}
	}
	if len(e.Args) != want {
		return Type{}, fmt.Errorf("%s 需要 %d 个参数", e.Op, want)
	}
	if !units[e.Name] {
		return Type{}, fmt.Errorf("%s: 单位 %q 不对", e.Op, e.Name)
	}
	if e.Zone != "" {
		if _, err := loadZone(e.Zone); err != nil {
			return Type{}, fmt.Errorf("%s: 时区 %q: %v", e.Op, e.Zone, err)
		}
	}
	x, err := p.typeOf(e.Args[0], cur)
	if err != nil {
		return Type{}, err
	}
	if x.Kind != Date && x.Kind != Timestamp {
		return Type{}, fmt.Errorf("%s: 只能用于日期 / 时间,实际是 %s", e.Op, x.Kind)
	}
	if x.Kind == Date && (e.Name == Minute || e.Name == Hour) {
		return Type{}, fmt.Errorf("%s: 日期只能加减天 / 月", e.Op)
	}
	if e.Op == "add" {
		n, err := p.typeOf(e.Args[1], cur)
		if err != nil {
			return Type{}, err
		}
		if n.Kind != Int32 && n.Kind != Int64 {
			return Type{}, fmt.Errorf("add: 个数必须是整数,实际是 %s", n.Kind)
		}
		x.Nullable = x.Nullable || n.Nullable
	}
	return x, nil
}

// timeOpEval: add / trunc 的参考实现(日历: timeops.go 的 addTime / truncTime)。
func timeOpEval(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
	t, err := p.typeOf(e.Args[0], schema)
	if err != nil {
		return nil, err
	}
	x, err := p.eval(e.Args[0], r, schema, params)
	if err != nil || x == nil {
		return nil, err
	}
	if e.Op == "trunc" {
		return truncTime(x.(time.Time), e.Name, t.Kind, e.Zone)
	}
	n, err := p.eval(e.Args[1], r, schema, params)
	if err != nil || n == nil {
		return nil, err
	}
	k := n.(int64)
	if e.Neg {
		k = -k
	}
	return addTime(x.(time.Time), k, e.Name, t.Kind, e.Zone)
}

func zoneName(z string) string {
	if z == "" {
		return "UTC"
	}
	return z
}

// intArg: add 的个数(make_interval 的参数是 int4)。常量直接写数字;int8 的表达式 cast 成 int4(超出范围 → 查询失败,和参考求值器一致)。
func (g *sqlGen) intArg(e *Expr, lv *sqlLevel) (string, error) {
	if e.Op == "const" {
		if v, ok := e.Value.(int64); ok {
			return fmt.Sprintf("(%d)", v), nil
		}
	}
	s, err := g.expr(e, lv)
	if err != nil {
		return "", err
	}
	if t, _ := g.p.typeOf(e, lv.in); t.Kind == Int64 {
		s = "(" + s + ")::int4"
	}
	return s, nil
}
