package core

// local_date: Args[0](时间)在领域的时区(Zone;空 = UTC)里的日期(墙上时间的年月日,结果是日期)。NULL → NULL。
// 设计稿的 今天 = local_date(now())(请求开始的时刻在领域的时区里是哪一天;测试的时钟 X-Factory-Now 一样适用)。
// 结果是日期: 可以和日期的列比较、加减天 / 月、截到月初;整数编码的日期的列经过 ymd 比较。
// 参考实现: time.In(时区).Date()(timeops.go 的 loadZone)。
// SQL: ((x AT TIME ZONE 'zone')::date)(timestamptz AT TIME ZONE → 那个时区的墙上时间;不受会话时区影响)。

import (
	"fmt"
	"time"
)

// LocalDate: 时间 x 在时区 zone 里的日期。
func LocalDate(x *Expr, zone string) *Expr {
	return &Expr{Op: "local_date", Args: []*Expr{x}, Zone: zone}
}

// Today: 今天(请求开始的时刻在领域的时区里的日期)。
func Today(zone string) *Expr { return LocalDate(Now(), zone) }

func init() {
	registerExpr(exprPrim{
		op: "local_date",
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			if len(e.Args) != 1 {
				return Type{}, fmt.Errorf("local_date 需要 1 个参数")
			}
			if e.Zone != "" {
				if _, err := loadZone(e.Zone); err != nil {
					return Type{}, fmt.Errorf("local_date: 时区 %q: %v", e.Zone, err)
				}
			}
			x, err := p.typeOf(e.Args[0], cur)
			if err != nil {
				return Type{}, err
			}
			if x.Kind != Timestamp {
				return Type{}, fmt.Errorf("local_date: 只能用于时间,实际是 %s", x.Kind)
			}
			return Type{Kind: Date, Nullable: x.Nullable}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			x, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || x == nil {
				return nil, err
			}
			loc, err := loadZone(e.Zone)
			if err != nil {
				return nil, err
			}
			y, m, d := x.(time.Time).In(loc).Date()
			return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			x, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("((%s AT TIME ZONE %s)::date)", x, quoteLit(zoneName(e.Zone))), nil
		},
	})
}
