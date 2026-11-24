// Package core 是工厂的核心代数: 基本元件(封闭集合)的定义。
//
// 设计稿: docs/design/core-algebra.md
//
// 只有这里定义的元件有实现,而且只有两份:
//   - SQL 后端(sql.go 的驱动 + 每个元件的 sql 钩子): 给服务器
//   - 参考求值器(eval.go 的驱动 + 每个元件的 eval 钩子): 给测试,在内存里逐行计算,独立于 SQL
//
// 每个元件一个文件(prim_<kind>.go / expr_<op>.go),三份实现(检查 / 参考求值 / SQL)放在一起,
// 由 registry.go 的登记表接到共享的驱动上。加元件: docs/adding-a-primitive.md
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
	// Rows: 子行的列表(nest 加的列)。Type.Fields = 每个子行的列。不能比较、排序、分组;值是 Children
	Rows Kind = "rows"
)

type Type struct {
	Kind     Kind        `json:"kind"`
	Nullable bool        `json:"nullable,omitempty"`
	Fields   []NamedType `json:"fields,omitempty"` // Kind = rows: 子行的列(按顺序)
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
	case JSON, Rows:
		return "jsonb"
	}
	panic(fmt.Sprintf("core: unknown kind %q", k))
}

func comparable(a, b Kind) bool {
	if a == b {
		return a != JSON && a != Rows
	}
	return (a == Int32 || a == Int64) && (b == Int32 || b == Int64)
}

// ---------------------------------------------------------------------------
// 表达式元件
// ---------------------------------------------------------------------------

// Expr: 表达式元件。Op = 元件名;每个元件的语义(NULL 规则)、类型规则、参考实现、SQL 写在 expr_<op>.go
// (登记表: registry.go):
//
//	col / const / param / sql_ref   expr_ref.go
//	cmp                             expr_cmp.go
//	and / or / not / is_null        expr_logic.go
//	absent                          expr_absent.go
//	in_list                         expr_in_list.go
//	starts_with / contains          expr_starts_with.go / expr_contains.go
//	json_field                      expr_json_field.go
//	tuple_after / in_tuples         expr_tuple_after.go / expr_in_tuples.go
//	now / add / trunc / ymd         expr_now.go / expr_time.go / expr_ymd.go
type Expr struct {
	Op     string   `json:"op"`
	Name   string   `json:"name,omitempty"`
	Value  any      `json:"value,omitempty"`
	Type   *Type    `json:"type,omitempty"`
	Args   []*Expr  `json:"args,omitempty"`
	Params []string `json:"params,omitempty"`
	Desc   bool     `json:"desc,omitempty"`
	Neg    bool     `json:"neg,omitempty"`  // add: 减
	Zone   string   `json:"zone,omitempty"` // add / trunc: 领域的时区(IANA 名;空 = UTC)
	// Loose: 只给差分测试的"错误实现"用(参考求值器认,SQL 后端和 IR 里没有)。ymd: 不合法的日期按日历进位
	// (20260230 → 2026-03-02、20260100 → 2025-12-31、20261301 → 2027-01-01;不检查就 make_date / time.Date 的实现)
	Loose bool `json:"-"`
}

// UsesNow: 表达式里有没有 now()。
func UsesNow(e *Expr) bool {
	if e == nil {
		return false
	}
	if e.Op == "now" {
		return true
	}
	for _, a := range e.Args {
		if UsesNow(a) {
			return true
		}
	}
	return false
}

// PipelineUsesNow: 流水线里有没有 now()(适配器要接时钟、测试要固定 now)。
func (p *Pipeline) UsesNow() bool {
	for _, st := range p.Steps {
		if UsesNow(st.Pred) || (st.AsOf != nil && UsesNow(st.AsOf.Ref)) {
			return true
		}
		for _, x := range st.Project {
			if UsesNow(x.Expr) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 关系元件(流水线的一步)
// ---------------------------------------------------------------------------

// Step: 关系元件(流水线的一步)。Kind = 元件名;每个元件的语义、检查(DRC)、参考实现、SQL 写在 prim_<kind>.go
// (登记表: registry.go):
//
//	scan / filter / project / pick_per_key / sort / limit   prim_<kind>.go
//	lookup / nest / child_agg                               prim_<kind>.go;对方是逻辑表(Source)、子表: relation.go
//	group / rank                                            prim_group.go / prim_rank.go(聚合: agg.go)
//
// 跨步骤的规则(scan 在第一步、sort 之后只能接 limit、limit 之前必须有 sort)在 check.go 的 Check。
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
	Aggs    []Agg       `json:"aggs,omitempty"`  // group / child_agg: 聚合(agg.go)
	As      string      `json:"as,omitempty"`    // rank: 名次的列名;nest: 子行的列表的列名
	From    string      `json:"from,omitempty"`  // 来自哪个单元 / 设计稿哪一行(审查和报错用)
	// lookup(Table / Columns 和 scan 一样是对方的表和列)
	On      []JoinKey  `json:"on,omitempty"`      // 当前行的列 = 对方的键列
	Take    []TakeCol  `json:"take,omitempty"`    // 加到当前行上的列(新列名 ← 对方的列)
	Missing string     `json:"missing,omitempty"` // keep(空 = keep)/ drop
	Unique  [][]string `json:"unique,omitempty"`  // 对方的表的唯一键(主键 / 唯一约束;DRC 用)
	// NullEq: 只给差分测试的"错误实现"用(NULL 当作相等的关联),参考求值器认,SQL 后端和 IR 里没有
	NullEq bool `json:"-"`
	// AsOf: lookup 按日期查历史表(lookup_asof.go): 键相等、开始 ≤ 参照(有结束时 参照 < 结束)的行里开始最大的一行。
	// lookup 的 Pred = 对方的列上的条件(对方的列 = 常量 …),先筛对方的行
	AsOf *AsOf `json:"as_of,omitempty"`
	// nest / child_agg(Table / Columns / On / Unique 同 lookup;On 的 Key = 子表的列。Pred = 子表的列上的条件;
	// Order = 子行的顺序;N = 子行的上限;Take = 子行的列)
	// (Aggs / As 见上: child_agg 的聚合、nest 的列名)
	// Source: 对方是逻辑表时,对方的行 = 这条子流水线的结果(见上)
	Source []Step `json:"source,omitempty"`
	// State: pick_per_key 的当前状态表(state.go): SQL 后端从这张表读每键最新(触发器维护),参考求值器照常取最新
	State string `json:"state,omitempty"`
	// Mutant: 只给差分测试的"错误实现"用(参考求值器认,SQL 后端和 IR 里没有):
	// nest: empty_null(空列表 → NULL);child_agg: sum_empty_null(空集合的 sum → NULL)/ count_outer(空集合的 count = 1,
	// LEFT JOIN + GROUP BY 的 count(*))
	Mutant string `json:"-"`
}

// JoinKey: 关联条件的一对列(列 = 列的等值)。
type JoinKey struct {
	Col string `json:"col"` // 当前行的列
	Key string `json:"key"` // 对方的键列
}

// TakeCol: lookup 加到当前行上的一列。
type TakeCol struct {
	Name string `json:"name"` // 新列名
	Col  string `json:"col"`  // 对方的列
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

// Children: rows 类型的值(nest 的子行的列表)。Rows 最多 Max+1 行(两个后端都多取一行);Max < 0 = 不知道上限
// (从 Postgres 读回的值)。比较(Canonical)只看 Rows。
type Children struct {
	Rows []Row
	Max  int
}

// Over: 子行多于上限(整个请求 FAILED_PRECONDITION)。
func (c Children) Over() bool { return c.Max >= 0 && len(c.Rows) > c.Max }

// Overflow: 这一行里有没有超过上限的子行的列表(出口判定 FAILED_PRECONDITION 用)。
func Overflow(r Row) bool {
	for _, v := range r {
		if c, ok := v.(Children); ok && c.Over() {
			return true
		}
	}
	return false
}

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
	var steps func(ss []Step) error
	steps = func(ss []Step) error {
		for i := range ss {
			if err := walk(ss[i].Pred); err != nil {
				return err
			}
			for _, x := range ss[i].Project {
				if err := walk(x.Expr); err != nil {
					return err
				}
			}
			if err := steps(ss[i].Source); err != nil {
				return err
			}
		}
		return nil
	}
	return steps(p.Steps)
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
