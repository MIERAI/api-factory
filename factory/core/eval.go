package core

// 参考求值器: 在内存里逐行计算流水线。独立于 SQL 后端,语义严格照 core.go 的注释。
//
// 值的表示: nil = NULL(bool 的 nil = UNKNOWN);int64;string;bool;*big.Rat(decimal);
// time.Time(date = UTC 零点;timestamp);json = encoding/json 解码的值(数字为 json.Number)。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Row map[string]any

// Params: 请求参数。没有这个键 = 没有指定。列表参数是 []any。
type Params map[string]any

type Result struct {
	Columns Schema
	Rows    []Row
	Ordered bool // 最后有 sort: 行的顺序有意义
}

// ErrAmbiguous: 排序 / 每键取一条遇到并列,而并列的行内容不同 → 结果不确定(数据或设计的问题,不是实现的问题)。
var ErrAmbiguous = errors.New("ambiguous order")

// Eval: tables = 物理表的行(scan 的输入)。
func Eval(p *Pipeline, tables map[string][]Row, params Params) (*Result, error) {
	schema := Schema{}
	var rows []Row
	ordered := false
	for _, st := range p.Steps {
		switch st.Kind {
		case "scan":
			schema = append(Schema(nil), st.Columns...)
			for _, r := range tables[st.Table] {
				row := Row{}
				for _, c := range st.Columns {
					row[c.Name] = r[c.Name]
				}
				rows = append(rows, row)
			}
		case "filter":
			var out []Row
			for _, r := range rows {
				v, err := p.eval(st.Pred, r, schema, params)
				if err != nil {
					return nil, err
				}
				if v == true {
					out = append(out, r)
				}
			}
			rows = out
		case "project":
			var next Schema
			for _, x := range st.Project {
				t, err := p.typeOf(x.Expr, schema)
				if err != nil {
					return nil, err
				}
				next = append(next, NamedType{x.Name, t})
			}
			var out []Row
			for _, r := range rows {
				row := Row{}
				for _, x := range st.Project {
					v, err := p.eval(x.Expr, r, schema, params)
					if err != nil {
						return nil, err
					}
					row[x.Name] = v
				}
				out = append(out, row)
			}
			schema, rows = next, out
		case "pick_per_key":
			groups := map[string][]Row{}
			var order []string
			for _, r := range rows {
				k := keyString(r, st.Keys)
				if _, ok := groups[k]; !ok {
					order = append(order, k)
				}
				groups[k] = append(groups[k], r)
			}
			var out []Row
			for _, k := range order {
				g := groups[k]
				sortRows(g, st.Order)
				if len(g) > 1 && compareRows(g[0], g[1], st.Order) == 0 && !sameRow(g[0], g[1]) {
					return nil, fmt.Errorf("%w: pick_per_key 的键 %s 有并列的行", ErrAmbiguous, k)
				}
				out = append(out, g[0])
			}
			rows = out
		case "sort":
			sortRows(rows, st.Order)
			for i := 1; i < len(rows); i++ {
				if compareRows(rows[i-1], rows[i], st.Order) == 0 && !sameRow(rows[i-1], rows[i]) {
					return nil, fmt.Errorf("%w: 排序 %v 有并列的行", ErrAmbiguous, st.Order)
				}
			}
			ordered = true
		case "limit":
			n := st.N
			if st.Param != "" {
				v, _ := params[st.Param].(int64)
				n = int(v)
			}
			if n < 0 {
				return nil, fmt.Errorf("limit: 负数 %d", n)
			}
			if len(rows) > n {
				rows = rows[:n]
			}
		default:
			return nil, fmt.Errorf("eval: unknown step %q", st.Kind)
		}
	}
	return &Result{Columns: schema, Rows: rows, Ordered: ordered}, nil
}

func keyString(r Row, keys []string) string {
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s\x00", Canonical(r[k]))
	}
	return b.String()
}

func sameRow(a, b Row) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if Canonical(v) != Canonical(b[k]) {
			return false
		}
	}
	return true
}

// sortRows: 稳定排序。NULL 视为比任何值都大(ASC 最后,DESC 最前),与 Postgres 默认一致。
func sortRows(rows []Row, order []SortKey) {
	sort.SliceStable(rows, func(i, j int) bool { return compareRows(rows[i], rows[j], order) < 0 })
}

func compareRows(a, b Row, order []SortKey) int {
	for _, o := range order {
		c := compareNullsLarge(a[o.Col], b[o.Col])
		if o.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func compareNullsLarge(a, b any) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return compareValues(a, b)
}

// compareValues: 两个非 NULL 值。文本按字节。
func compareValues(a, b any) int {
	switch x := a.(type) {
	case int64:
		y := b.(int64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case *big.Rat:
		return x.Cmp(b.(*big.Rat))
	case string:
		return bytes.Compare([]byte(x), []byte(b.(string)))
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	case time.Time:
		return x.Compare(b.(time.Time))
	}
	panic(fmt.Sprintf("core: cannot compare %T", a))
}

// absent: 标量 = 没有这个键或 nil;列表 = 没有这个键、nil 或空列表。
func absent(params Params, name string) bool {
	v, ok := params[name]
	if !ok || v == nil {
		return true
	}
	if l, isList := v.([]any); isList {
		return len(l) == 0
	}
	return false
}

func (p *Pipeline) eval(e *Expr, r Row, schema Schema, params Params) (any, error) {
	switch e.Op {
	case "col":
		return r[e.Name], nil
	case "const":
		return e.Value, nil
	case "param":
		if absent(params, e.Name) {
			return nil, nil
		}
		return params[e.Name], nil
	case "cmp":
		a, err := p.eval(e.Args[0], r, schema, params)
		if err != nil {
			return nil, err
		}
		b, err := p.eval(e.Args[1], r, schema, params)
		if err != nil {
			return nil, err
		}
		if a == nil || b == nil {
			return nil, nil
		}
		c := compareValues(a, b)
		switch e.Name {
		case "=":
			return c == 0, nil
		case "<>":
			return c != 0, nil
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		case ">=":
			return c >= 0, nil
		}
	case "and", "or":
		// Kleene: and = 有 FALSE 则 FALSE,否则有 UNKNOWN 则 UNKNOWN;or 对称
		dominant := e.Op == "or" // or: TRUE 占优;and: FALSE 占优
		unknown := false
		for _, x := range e.Args {
			v, err := p.eval(x, r, schema, params)
			if err != nil {
				return nil, err
			}
			if v == nil {
				unknown = true
			} else if v.(bool) == dominant {
				return dominant, nil
			}
		}
		if unknown {
			return nil, nil
		}
		return !dominant, nil
	case "not":
		v, err := p.eval(e.Args[0], r, schema, params)
		if err != nil || v == nil {
			return nil, err
		}
		return !v.(bool), nil
	case "is_null":
		v, err := p.eval(e.Args[0], r, schema, params)
		if err != nil {
			return nil, err
		}
		return v == nil, nil
	case "absent":
		return absent(params, e.Name), nil
	case "in_list":
		a, err := p.eval(e.Args[0], r, schema, params)
		if err != nil || a == nil {
			return nil, err
		}
		list, _ := params[e.Name].([]any)
		for _, x := range list {
			if compareValues(a, x) == 0 {
				return true, nil
			}
		}
		return false, nil
	case "starts_with":
		a, err := p.eval(e.Args[0], r, schema, params)
		if err != nil || a == nil || absent(params, e.Name) {
			return nil, err
		}
		return strings.HasPrefix(a.(string), params[e.Name].(string)), nil
	case "json_field":
		a, err := p.eval(e.Args[0], r, schema, params)
		if err != nil || a == nil {
			return nil, err
		}
		return jsonField(a, e.Name, e.Type.Kind)
	case "tuple_after":
		if absent(params, e.Params[0]) {
			return true, nil
		}
		for i, c := range e.Args {
			v, err := p.eval(c, r, schema, params)
			if err != nil {
				return nil, err
			}
			cmp := compareValues(v, params[e.Params[i]])
			if e.Desc {
				cmp = -cmp
			}
			if cmp != 0 {
				return cmp > 0, nil
			}
		}
		return false, nil
	}
	return nil, fmt.Errorf("eval: unknown expr %q", e.Op)
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

var errCast = errors.New("invalid input")

// castText: 模仿 Postgres 的文本 → 类型转换(只覆盖规范的写法;其他一律视为格式错)。
func castText(s string, k Kind) (any, error) {
	fail := func() (any, error) { return nil, fmt.Errorf("%w for type %s: %q", errCast, k.SQLType(), s) }
	t := strings.Trim(s, " \t\n\r\f\v") // 和 Postgres 一样只跳过 ASCII 的空白(全角空格不算)
	switch k {
	case Text:
		return s, nil
	case Int32, Int64:
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil || (k == Int32 && (n > math.MaxInt32 || n < math.MinInt32)) {
			return fail()
		}
		return n, nil
	case Decimal:
		if t == "" || strings.ContainsAny(t, "eE/") || strings.EqualFold(t, "nan") || strings.Contains(strings.ToLower(t), "inf") {
			return fail()
		}
		r, ok := new(big.Rat).SetString(t)
		if !ok {
			return fail()
		}
		return r, nil
	case Bool:
		switch strings.ToLower(t) {
		case "t", "true", "y", "yes", "on", "1":
			return true, nil
		case "f", "false", "n", "no", "off", "0":
			return false, nil
		}
		return fail()
	case Date:
		d, err := time.Parse("2006-01-02", t)
		if err != nil {
			return fail()
		}
		return d, nil
	case Timestamp:
		ts, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			return fail()
		}
		return ts.UTC(), nil
	}
	return fail()
}

// Canonical: 值的规范文本(比较结果用)。NULL = "∅"。
func Canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return "∅"
	case int64:
		return strconv.FormatInt(x, 10)
	case *big.Rat:
		return RatString(x)
	case string:
		return strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// RatString: 有理数的十进制文本(去掉多余的 0)。只用于有限小数。
func RatString(r *big.Rat) string {
	s := r.FloatString(20)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" {
		s = "0"
	}
	return s
}

// FromPGText: Postgres 的 ::text 输出(会话时区 UTC)→ 核心的值。nil = NULL。
func FromPGText(s *string, k Kind) (any, error) {
	if s == nil {
		return nil, nil
	}
	switch k {
	case Bool:
		return *s == "true", nil
	case JSON:
		dec := json.NewDecoder(strings.NewReader(*s))
		dec.UseNumber()
		var v any
		err := dec.Decode(&v)
		return v, err
	case Timestamp:
		for _, layout := range []string{"2006-01-02 15:04:05.999999Z07", "2006-01-02 15:04:05Z07"} {
			if t, err := time.Parse(layout, *s); err == nil {
				return t.UTC(), nil
			}
		}
		return nil, fmt.Errorf("timestamp %q", *s)
	}
	return castText(*s, k)
}
