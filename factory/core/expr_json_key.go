package core

// json_key: Args[0](json)里键 Name 的值作为**索引的键**: 是类型 Type 的规范写法(jsonFormats,和 json_field 同一个正则)
// 并且能转换 → 转换后的值;键不存在 / JSON null / 不是规范写法 / 超出范围 / 不存在的日期 → NULL。**永远不报错**。
//
// 用途: JSON版本 的表的键上的等值条件(store_code = $1)用得上表达式索引。json_field 遇到不合法的值要让查询失败
// (CASE … ELSE '✗' || 值 END)::类型 —— 这个式子放进索引的话,脏数据连写都写不进去;所以索引和条件都用这个
// 不报错的式子,两边由同一个函数 JSONKeySQL 生成(索引的 DDL: JSONKeyIndexSQL),不会走样。
// 合法的值上 json_key = json_field(同一个正则 + 同一个转换);不合法的值上 json_key 是 NULL,json_field 报错。
// 所以 keyindex.go 只把 json_key 的条件**加在**原来的条件旁边(原来的条件留着,读到的行照样校验)。
//
// SQL(v = (x ->> 'key'),P = jsonFormats 的正则;CASE 是按顺序求值的,里层的转换只在外层的检查通过之后才算):
//
//	文本    (v::text)                                            ← 和 json_field 完全相同
//	整数    CASE WHEN v ~ P THEN CASE WHEN v::numeric BETWEEN 下限 AND 上限 THEN v::int4 / int8 END END
//	十进制  CASE WHEN v ~ P THEN v::numeric END
//	真假    CASE WHEN v ~* P THEN v::bool END
//	日期    CASE WHEN v ~ P THEN CASE WHEN 年 ≥ 1 AND 月 1~12 THEN CASE WHEN 日 1~月末 THEN make_date(年, 月, 日) END END END
//	时刻    不支持(text → timestamptz 不是 IMMUTABLE,带时区偏移的写法也不能逐字比较)→ 不生成,条件保持 json_field
//
// 每个式子只用 IMMUTABLE 的函数(text::date 是 STABLE,所以日期用 make_date 拼),可以放进表达式索引。

import (
	"fmt"
	"strconv"
	"time"
)

// JSONKey: x 里键 key 的值作为索引的键(类型 t)。
func JSONKey(x *Expr, key string, t Type) *Expr {
	return &Expr{Op: "json_key", Name: key, Type: &t, Args: []*Expr{x}}
}

func init() {
	registerExpr(exprPrim{
		op: "json_key",
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			a, err := p.typeOf(e.Args[0], cur)
			if err != nil {
				return Type{}, err
			}
			if a.Kind != JSON || e.Type == nil {
				return Type{}, fmt.Errorf("json_key: 输入必须是 json,需要类型")
			}
			if _, ok := JSONKeySQL("v", e.Type.Kind); !ok {
				return Type{}, fmt.Errorf("json_key: 类型 %s 不能做索引的键", e.Type.Kind)
			}
			return Type{Kind: e.Type.Kind, Nullable: true}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			a, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || a == nil {
				return nil, err
			}
			return jsonKey(a, e.Name, e.Type.Kind), nil
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			a, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			s, ok := JSONKeySQL(fmt.Sprintf("(%s ->> %s)", a, quoteLit(e.Name)), e.Type.Kind)
			if !ok {
				return "", fmt.Errorf("json_key: 类型 %s 不能做索引的键", e.Type.Kind)
			}
			return s, nil
		},
		nullRejects: firstArgRejects,
	})
}

// jsonKey: 参考实现。json_field 的转换报错的地方 → NULL(日期: 年 0 也是 NULL,和 SQL 的 make_date 一致)。
func jsonKey(doc any, key string, k Kind) any {
	v, err := jsonField(doc, key, k)
	if err != nil {
		return nil
	}
	if t, ok := v.(time.Time); ok && k == Date && t.Year() < 1 {
		return nil
	}
	return v
}

// JSONKeySQL: 索引的键的 SQL(v = 取出文本的式子,例如 (t1.data ->> 'store_code'))。
// 查询的条件(json_key 的 SQL)和索引的 DDL(JSONKeyIndexSQL)都从这里生成。ok = false: 这个类型不能做索引的键。
func JSONKeySQL(v string, k Kind) (string, bool) {
	f := jsonFormats[k]
	re := func() string {
		if f.ci {
			return fmt.Sprintf("%s ~* %s", v, quoteLit(f.pattern))
		}
		return fmt.Sprintf("%s ~ %s", v, quoteLit(f.pattern))
	}
	switch k {
	case Text:
		return fmt.Sprintf("(%s::text)", v), true
	case Int32, Int64:
		lo, hi := strconv.Itoa(-1<<31), strconv.Itoa(1<<31-1)
		if k == Int64 {
			lo, hi = "-9223372036854775808", "9223372036854775807"
		}
		return fmt.Sprintf("(CASE WHEN %s THEN (CASE WHEN (%s::numeric BETWEEN %s AND %s) THEN (%s::%s) END) END)", re(), v, lo, hi, v, k.SQLType()), true
	case Decimal:
		return fmt.Sprintf("(CASE WHEN %s THEN (%s::numeric) END)", re(), v), true
	case Bool:
		return fmt.Sprintf("(CASE WHEN %s THEN (%s::bool) END)", re(), v), true
	case Date:
		// 规范写法里唯一的 YYYY-MM-DD(前后只有空白)
		d := fmt.Sprintf("substring(%s from '[0-9]{4}-[0-9]{2}-[0-9]{2}')", v)
		y, m, dd := fmt.Sprintf("substr(%s, 1, 4)::int4", d), fmt.Sprintf("substr(%s, 6, 2)::int4", d), fmt.Sprintf("substr(%s, 9, 2)::int4", d)
		// 月末 = 下个月 1 日 - 这个月 1 日(date - date = 天数)
		last := fmt.Sprintf("(make_date(%s + %s / 12, %s %% 12 + 1, 1) - make_date(%s, %s, 1))", y, m, m, y, m)
		return fmt.Sprintf("(CASE WHEN %s THEN (CASE WHEN ((%s >= 1) AND (%s BETWEEN 1 AND 12)) THEN (CASE WHEN (%s BETWEEN 1 AND %s) THEN make_date(%s, %s, %s) END) END) END)",
			re(), y, m, dd, last, y, m, dd), true
	}
	return "", false
}

// JSONKeyIndexSQL: JSON 列 dataCol 里键 key(类型 k)的表达式索引的式子(CREATE INDEX … ON 表 (这个, …))。
// 和 json_key 在查询里的式子逐字相同(只是不带表的别名)。
func JSONKeyIndexSQL(dataCol, key string, k Kind) (string, bool) {
	return JSONKeySQL(fmt.Sprintf("(%s ->> %s)", quoteIdent(dataCol), quoteLit(key)), k)
}
