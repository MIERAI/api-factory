package core

// ymd: Args[0](整数)按 YYYYMMDD 编码读成日期(旧的表结构常见: 20260131 = 2026-01-31)。
// 不是合法的日期(20260230、20261301、0、负数、1000 年之前)→ NULL(比较是 UNKNOWN: 永远不命中)。NULL → NULL。
// 参考实现: timeops.go 的 YMDDate(Expr.Loose: 差分测试的"错误实现",按日历进位)。
// SQL 后端: 在条件的肯定位置(filter 里,不在 not 之下)的 cmp(op, ymd(x), v) 写成 x op <v 的 YYYYMMDD 整数>
// AND <x 是合法的日期>(按列本来的编码比较: 整数列上的索引能用;expr_cmp.go 调 ymdCmp);其他位置按 CASE 换算

import "fmt"

// YMD: 整数(YYYYMMDD)→ 日期(不合法 → NULL)。
func YMD(x *Expr) *Expr { return &Expr{Op: "ymd", Args: []*Expr{x}} }

func init() {
	registerExpr(exprPrim{
		op: "ymd",
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			if len(e.Args) != 1 {
				return Type{}, fmt.Errorf("ymd 需要 1 个参数")
			}
			x, err := p.typeOf(e.Args[0], cur)
			if err != nil {
				return Type{}, err
			}
			if x.Kind != Int32 && x.Kind != Int64 {
				return Type{}, fmt.Errorf("ymd: 只能用于整数(YYYYMMDD),实际是 %s", x.Kind)
			}
			return Type{Kind: Date, Nullable: true}, nil // 不合法的整数 → NULL
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			x, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || x == nil {
				return nil, err
			}
			return YMDDate(x.(int64), e.Loose), nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			x, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			// 外层的 CASE 先确认年月的范围,内层才用 make_date(不合法的月不会传给 make_date;Postgres 的 CASE 化简不会先算没有走到的分支)
			return fmt.Sprintf("(CASE WHEN %s THEN make_date((%s / 10000)::int4, (%s / 100 %% 100)::int4, (%s %% 100)::int4) END)", ymdValidSQL(x), x, x, x), nil
		},
	})
}

// ymdValidSQL: 整数 x 是合法的 YYYYMMDD(年 1000~9999、月 1~12、日 1~那个月的天数)。不会报错: 月不合法时不算那个月的天数。
func ymdValidSQL(x string) string {
	return fmt.Sprintf("(CASE WHEN %s BETWEEN 10000101 AND 99991231 AND %s / 100 %% 100 BETWEEN 1 AND 12 THEN %s %% 100 BETWEEN 1 AND "+
		"extract(day from make_date((%s / 10000)::int4, (%s / 100 %% 100)::int4, 1) + interval '1 month' - interval '1 day')::int4 ELSE FALSE END)", x, x, x, x, x)
}

// ymdCmp: 肯定位置的 cmp(op, ymd(x), v) → (x op <v 的 YYYYMMDD>) AND <x 合法>。
// 合法的 YYYYMMDD 的整数顺序 = 日期的顺序(年 1000~9999 都是 8 位),所以合法的 x 上两者相同;
// 不合法的 x: 原来是 UNKNOWN,改写后是 FALSE —— 在肯定位置效果一样(不留下这一行)。v 为 NULL → 两边都不是 TRUE。
// 比较按 x 本来的编码(整数)进行: x 是列时,列上的 btree 索引能用(v 的整数在执行前就算好了)。
func (g *sqlGen) ymdCmp(e *Expr, lv *sqlLevel) (string, error) {
	x, err := g.expr(e.Args[0].Args[0], lv)
	if err != nil {
		return "", err
	}
	v, err := g.expr(e.Args[1], lv)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("((%s %s (to_char(%s, 'YYYYMMDD'))::int4) AND %s)", x, e.Name, v, ymdValidSQL(x)), nil
}
