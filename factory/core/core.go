// Package core 是工厂的核心代数: 基本元件(封闭集合)的定义。
//
// 设计稿: docs/design/core-algebra.md
//
// 只有这里定义的元件有实现,而且只有两份:
//   - SQL 后端(sql.go): 给服务器
//   - 参考求值器(eval.go): 给测试,在内存里逐行计算,独立于 SQL
//
// 单元库(属于、有效期包含、多条……)只是这些元件的组合,不带实现。
// 新增元件要单独评审,并且必须通过元件认证(cert_test.go)。
package core

import (
	"fmt"
	"math/big"
	"time"
)

// ---------------------------------------------------------------------------
// 类型
// ---------------------------------------------------------------------------

type Kind string

const (
	Int32     Kind = "int32"
	Int64     Kind = "int64"
	Text      Kind = "text"
	Bool      Kind = "bool"
	Decimal   Kind = "decimal"   // 精确的十进制(SQL numeric;参考实现 big.Rat;API 里是十进制字符串)
	Date      Kind = "date"      // 日期(API 里 YYYY-MM-DD)
	Timestamp Kind = "timestamp" // 时刻(API 里 RFC3339 UTC)
	JSON      Kind = "json"      // 只能作为 json_field 的输入
)

type Type struct {
	Kind     Kind `json:"kind"`
	Nullable bool `json:"nullable,omitempty"`
}

// SQLType: SQL 里的类型名(参数的 cast 用)。
func (k Kind) SQLType() string {
	switch k {
	case Int32:
		return "int4"
	case Int64:
		return "int8"
	case Text:
		return "text"
	case Bool:
		return "bool"
	case Decimal:
		return "numeric"
	case Date:
		return "date"
	case Timestamp:
		return "timestamptz"
	case JSON:
		return "jsonb"
	}
	panic(fmt.Sprintf("core: unknown kind %q", k))
}

func comparable(a, b Kind) bool {
	if a == b {
		return a != JSON
	}
	return (a == Int32 || a == Int64) && (b == Int32 || b == Int64)
}

// ---------------------------------------------------------------------------
// 表达式元件
// ---------------------------------------------------------------------------

// Expr 的 Op:
//
//	col          Name = 列名
//	const        Value(Type 决定种类;nil = NULL)
//	param        Name = 参数名
//	cmp          Name ∈ = <> < <= > >=;Args[0], Args[1]。任一边 NULL → UNKNOWN
//	and / or     Args(≥ 2),Kleene 三值逻辑
//	not          Args[0]
//	is_null      Args[0];永远 TRUE / FALSE
//	absent       Name = 参数名: 标量没有指定 / 列表为空或没有指定。"不传"只在这里定义
//	in_list      Args[0] 属于参数 Name 的列表。Args[0] 为 NULL → UNKNOWN;空列表 → FALSE
//	starts_with  Args[0] 以参数 Name 开头(按字面,% _ \ 不是通配符);任一边 NULL → UNKNOWN
//	json_field   Args[0](json)里键 Name 的值转换成 Type。键不存在 / JSON null → NULL;格式错 → 查询失败
//	tuple_after  Args(列)按 Desc 方向严格在参数 Params 之后(keyset 游标)。参数 Params[0] 没有指定 → TRUE
type Expr struct {
	Op     string   `json:"op"`
	Name   string   `json:"name,omitempty"`
	Value  any      `json:"value,omitempty"`
	Type   *Type    `json:"type,omitempty"`
	Args   []*Expr  `json:"args,omitempty"`
	Params []string `json:"params,omitempty"`
	Desc   bool     `json:"desc,omitempty"`
}

func Col(name string) *Expr              { return &Expr{Op: "col", Name: name} }
func Param(name string) *Expr            { return &Expr{Op: "param", Name: name} }
func Const(v any, t Type) *Expr          { return &Expr{Op: "const", Value: v, Type: &t} }
func Cmp(op string, a, b *Expr) *Expr    { return &Expr{Op: "cmp", Name: op, Args: []*Expr{a, b}} }
func And(xs ...*Expr) *Expr              { return &Expr{Op: "and", Args: xs} }
func Or(xs ...*Expr) *Expr               { return &Expr{Op: "or", Args: xs} }
func Not(x *Expr) *Expr                  { return &Expr{Op: "not", Args: []*Expr{x}} }
func IsNull(x *Expr) *Expr               { return &Expr{Op: "is_null", Args: []*Expr{x}} }
func Absent(param string) *Expr          { return &Expr{Op: "absent", Name: param} }
func InList(x *Expr, param string) *Expr { return &Expr{Op: "in_list", Name: param, Args: []*Expr{x}} }
func StartsWith(x *Expr, param string) *Expr {
	return &Expr{Op: "starts_with", Name: param, Args: []*Expr{x}}
}
func JSONField(x *Expr, key string, t Type) *Expr {
	return &Expr{Op: "json_field", Name: key, Type: &t, Args: []*Expr{x}}
}
func TupleAfter(cols []string, params []string, desc bool) *Expr {
	e := &Expr{Op: "tuple_after", Params: params, Desc: desc}
	for _, c := range cols {
		e.Args = append(e.Args, Col(c))
	}
	return e
}

var cmpOps = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

// ---------------------------------------------------------------------------
// 关系元件(流水线的一步)
// ---------------------------------------------------------------------------

// Step 的 Kind:
//
//	scan          Table;Columns = 物理表的列(名字 → 类型)
//	filter        Pred: 只保留 TRUE 的行(UNKNOWN 丢弃)
//	project       Project: 新的列(名字 → 表达式),只保留这些列
//	pick_per_key  Keys: 每个键(NULL 视为相等)按 Order 取第一行
//	sort          Order: 文本按字节;NULL 在 ASC 时最后、DESC 时最前
//	limit         N;或者 Param = 参数名(必填的整数参数,例如分页的 row_limit)
type Step struct {
	Kind    string      `json:"kind"`
	Table   string      `json:"table,omitempty"`
	Columns []NamedType `json:"columns,omitempty"`
	Pred    *Expr       `json:"pred,omitempty"`
	Project []NamedExpr `json:"project,omitempty"`
	Keys    []string    `json:"keys,omitempty"`
	Order   []SortKey   `json:"order,omitempty"`
	N       int         `json:"n,omitempty"`
	Param   string      `json:"param,omitempty"` // limit: 行数来自参数
	From    string      `json:"from,omitempty"`  // 来自哪个单元 / 设计稿哪一行(审查和报错用)
}

type NamedType struct {
	Name string `json:"name"`
	Type Type   `json:"type"`
}

type NamedExpr struct {
	Name string `json:"name"`
	Expr *Expr  `json:"expr"`
}

type SortKey struct {
	Col  string `json:"col"`
	Desc bool   `json:"desc,omitempty"`
}

func Scan(table string, cols ...NamedType) Step {
	return Step{Kind: "scan", Table: table, Columns: cols}
}
func Filter(p *Expr) Step          { return Step{Kind: "filter", Pred: p} }
func Project(xs ...NamedExpr) Step { return Step{Kind: "project", Project: xs} }
func PickPerKey(keys []string, order ...SortKey) Step {
	return Step{Kind: "pick_per_key", Keys: keys, Order: order}
}
func Sort(order ...SortKey) Step { return Step{Kind: "sort", Order: order} }
func Limit(n int) Step           { return Step{Kind: "limit", N: n} }
func LimitParam(p string) Step   { return Step{Kind: "limit", Param: p} }

// Pipeline: 一条查询。Params = 请求参数的类型(列表参数 List = true)。
type Pipeline struct {
	Steps  []Step      `json:"steps"`
	Params []ParamDecl `json:"params"`
}

type ParamDecl struct {
	Name     string `json:"name"`
	Type     Type   `json:"type"` // 元素类型
	List     bool   `json:"list,omitempty"`
	Required bool   `json:"required,omitempty"` // 必填: absent 恒为 FALSE
}

func (p *Pipeline) Param(name string) *ParamDecl {
	for i := range p.Params {
		if p.Params[i].Name == name {
			return &p.Params[i]
		}
	}
	return nil
}

// Normalize: 流水线从 JSON(IR 文件)读回来后,按声明的类型还原常量的值
// (JSON 里整数会变成 float64,decimal / 时间会变成字符串)。
func (p *Pipeline) Normalize() error {
	var walk func(e *Expr) error
	walk = func(e *Expr) error {
		if e == nil {
			return nil
		}
		if e.Op == "const" && e.Value != nil && e.Type != nil {
			v, err := normalizeConst(e.Value, e.Type.Kind)
			if err != nil {
				return err
			}
			e.Value = v
		}
		for _, a := range e.Args {
			if err := walk(a); err != nil {
				return err
			}
		}
		return nil
	}
	for i := range p.Steps {
		if err := walk(p.Steps[i].Pred); err != nil {
			return err
		}
		for _, x := range p.Steps[i].Project {
			if err := walk(x.Expr); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeConst(v any, k Kind) (any, error) {
	switch x := v.(type) {
	case float64:
		if k == Int32 || k == Int64 {
			return int64(x), nil
		}
		return castText(fmt.Sprint(x), k)
	case string:
		if k == Text {
			return x, nil
		}
		if k == Decimal {
			if r, ok := new(big.Rat).SetString(x); ok { // big.Rat 的 JSON 是 "3/2"
				return r, nil
			}
		}
		if k == Timestamp {
			return castText(x, k)
		}
		if k == Date {
			if t, err := time.Parse(time.RFC3339, x); err == nil {
				return t, nil
			}
		}
		return castText(x, k)
	}
	return v, nil
}
