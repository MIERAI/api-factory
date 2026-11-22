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
	ev := &evaluator{p: p, tables: tables, params: params, schema: Schema{}}
	for _, st := range p.Steps {
		sp := stepPrims[st.Kind] // 每个元件的求值在 prim_<kind>.go
		if sp == nil {
			return nil, fmt.Errorf("eval: unknown step %q", st.Kind)
		}
		if err := sp.eval(ev, st); err != nil {
			return nil, err
		}
	}
	return &Result{Columns: ev.schema, Rows: ev.rows, Ordered: ev.ordered}, nil
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
	if xp := exprPrims[e.Op]; xp != nil && xp.eval != nil { // 每个元件的参考实现在 expr_<op>.go
		return xp.eval(p, e, r, schema, params)
	}
	return nil, fmt.Errorf("eval: unknown expr %q", e.Op)
}

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
	case Children: // 子行: 每行的列按名字排序(行里的列是 map)。上限不算(从 Postgres 读回的值不知道上限)
		var b strings.Builder
		b.WriteString("[")
		for i, r := range x.Rows {
			if i > 0 {
				b.WriteString(", ")
			}
			keys := make([]string, 0, len(r))
			for k := range r {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteString("{")
			for j, k := range keys {
				if j > 0 {
					b.WriteString(" ")
				}
				b.WriteString(k + "=" + Canonical(r[k]))
			}
			b.WriteString("}")
		}
		b.WriteString("]")
		return b.String()
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

// FromPGTyped: 和 FromPGText 一样,另外认 rows(nest 的子行的 JSON,按 t.Fields 的类型还原)。
func FromPGTyped(s *string, t Type) (any, error) {
	if t.Kind != Rows {
		return FromPGText(s, t.Kind)
	}
	if s == nil {
		return nil, nil
	}
	rows, err := ChildRowsFromJSON([]byte(*s), t.Fields)
	if err != nil {
		return nil, err
	}
	return Children{Rows: rows, Max: -1}, nil
}

// ChildRowsFromJSON: 子行的 JSON(核心的 API 表示: 十进制 = 文本,时间 = UTC 的 RFC3339 文本,日期 = YYYY-MM-DD)→ 行。
// 和适配器扫描子行用的是同一种表示(SQL 后端的 jsonValueSQL)。JSON 数字只允许整数(十进制如果变成了 JSON 数字 → 报错)。
func ChildRowsFromJSON(raw []byte, fields []NamedType) ([]Row, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var items []map[string]any
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("子行的 JSON: %w", err)
	}
	if items == nil {
		return nil, fmt.Errorf("子行的 JSON: 不是数组: %s", raw)
	}
	out := make([]Row, 0, len(items))
	for _, it := range items {
		r := Row{}
		for _, f := range fields {
			v, ok := it[f.Name]
			if !ok {
				return nil, fmt.Errorf("子行的 JSON: 没有键 %s", f.Name)
			}
			if v == nil {
				r[f.Name] = nil
				continue
			}
			var err error
			switch x := v.(type) {
			case json.Number:
				if f.Type.Kind != Int32 && f.Type.Kind != Int64 {
					return nil, fmt.Errorf("子行的 JSON: %s 是 %s,不能是 JSON 数字 %s", f.Name, f.Type.Kind, x)
				}
				r[f.Name], err = castText(string(x), f.Type.Kind)
			case bool:
				if f.Type.Kind != Bool {
					return nil, fmt.Errorf("子行的 JSON: %s 是 %s,不能是 bool", f.Name, f.Type.Kind)
				}
				r[f.Name] = x
			case string:
				switch f.Type.Kind {
				case Text, Decimal, Date, Timestamp:
					r[f.Name], err = castText(x, f.Type.Kind)
				default:
					return nil, fmt.Errorf("子行的 JSON: %s 是 %s,不能是文本 %q", f.Name, f.Type.Kind, x)
				}
			default:
				return nil, fmt.Errorf("子行的 JSON: %s 的值 %T 不支持", f.Name, v)
			}
			if err != nil {
				return nil, fmt.Errorf("子行的 JSON: %s: %w", f.Name, err)
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// Compare: 两个值的顺序(和排序一样的规则;NULL 最大)。验收的数据生成用。
func Compare(a, b any) int { return compareNullsLarge(a, b) }
