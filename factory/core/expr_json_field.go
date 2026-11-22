package core

// json_field: Args[0](json)里键 Name 的值转换成 Type。键不存在 / JSON null → NULL;格式错 → 查询失败。
// 两边只认规范的写法(jsonFormats: 参考求值器和 SQL 共用同一个正则)。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func JSONField(x *Expr, key string, t Type) *Expr {
	return &Expr{Op: "json_field", Name: key, Type: &t, Args: []*Expr{x}}
}

func init() {
	registerExpr(exprPrim{
		op: "json_field",
		typeOf: func(p *Pipeline, e *Expr, cur Schema) (Type, error) {
			a, err := p.typeOf(e.Args[0], cur)
			if err != nil {
				return Type{}, err
			}
			if a.Kind != JSON || e.Type == nil || e.Type.Kind == JSON {
				return Type{}, fmt.Errorf("json_field: 输入必须是 json,输出必须是标量类型")
			}
			return Type{Kind: e.Type.Kind, Nullable: true}, nil
		},
		eval: func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error) {
			a, err := p.eval(e.Args[0], r, schema, params)
			if err != nil || a == nil {
				return nil, err
			}
			return jsonField(a, e.Name, e.Type.Kind)
		},
		sql: func(g *sqlGen, e *Expr, lv *sqlLevel, _ bool) (string, error) {
			a, err := g.expr(e.Args[0], lv)
			if err != nil {
				return "", err
			}
			v := fmt.Sprintf("(%s ->> %s)", a, quoteLit(e.Name))
			f, ok := jsonFormats[e.Type.Kind]
			if !ok {
				return fmt.Sprintf("(%s::%s)", v, e.Type.Kind.SQLType()), nil
			}
			// 不是规范写法 → 拼成必然转换失败的文本,查询失败(和参考求值器一致;NULL 仍然是 NULL)
			op := "~"
			if f.ci {
				op = "~*"
			}
			return fmt.Sprintf("((CASE WHEN %s %s %s THEN %s ELSE '✗' || %s END)::%s)", v, op, quoteLit(f.pattern), v, v, e.Type.Kind.SQLType()), nil
		},
	})
}

// jsonField: 模仿 Postgres 的 (x ->> key)::T。
// 支持的输入: 对象里的字符串 / 数字(不含指数)/ bool / null。其他(嵌套对象、数组、指数)→ 报错(参考实现不支持)。
func jsonField(doc any, key string, k Kind) (any, error) {
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, nil // ->> 用在非对象上 → NULL
	}
	v, ok := obj[key]
	if !ok || v == nil {
		return nil, nil
	}
	var text string
	switch x := v.(type) {
	case string:
		text = x
	case json.Number:
		if strings.ContainsAny(string(x), "eE") {
			return nil, fmt.Errorf("json_field %s: 指数形式的数字参考实现不支持", key)
		}
		text = string(x)
	case bool:
		text = strconv.FormatBool(x)
	default:
		return nil, fmt.Errorf("json_field %s: 值 %T 参考实现不支持", key, v)
	}
	if f, ok := jsonFormats[k]; ok && !f.re.MatchString(text) {
		return nil, fmt.Errorf("%w for type %s: %q(不是规范的写法)", errCast, k.SQLType(), text)
	}
	return castText(text, k)
}

// jsonFormats: JSON 字段只接受的规范写法(参考求值器和 SQL 共用同一个正则;两边的正则语法在这些写法上一致)。
// Postgres 的 cast 本身很宽松("2026/01/10"、"20260110" 都能读成日期,"1e2" 能读成 100,"tr" 能读成 true),
// 参考求值器只认规范写法 —— 两边曾经不一致。现在 SQL 也先检查格式,不符合就让 cast 失败(查询失败)。
type jsonFormat struct {
	pattern string // 不区分大小写时 SQL 用 ~*
	ci      bool
	re      *regexp.Regexp
}

const ws = `[ \t\n\r\f\v]*` // Postgres 的数值输入会跳过的空白(ASCII)

var jsonFormats = func() map[Kind]*jsonFormat {
	m := map[Kind]*jsonFormat{
		Int32:     {pattern: `^` + ws + `[+-]?[0-9]+` + ws + `$`},
		Int64:     {pattern: `^` + ws + `[+-]?[0-9]+` + ws + `$`},
		Decimal:   {pattern: `^` + ws + `[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)` + ws + `$`},
		Bool:      {pattern: `^` + ws + `(t|true|y|yes|on|1|f|false|n|no|off|0)` + ws + `$`, ci: true},
		Date:      {pattern: `^` + ws + `[0-9]{4}-[0-9]{2}-[0-9]{2}` + ws + `$`},
		Timestamp: {pattern: `^` + ws + `[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,6})?(Z|[+-][0-9]{2}:[0-9]{2})` + ws + `$`},
	}
	for _, f := range m {
		p := f.pattern
		if f.ci {
			p = "(?i)" + p
		}
		f.re = regexp.MustCompile(p)
	}
	return m
}()
