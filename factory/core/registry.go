package core

// 元件的登记表: 每个元件在自己的文件里登记自己的实现,共享的驱动(Check / Eval / ToSQL 的循环)只按名字查表。
//
//	关系元件(流水线的一步)  prim_<kind>.go   registerStep(stepPrim{…})
//	表达式元件              expr_<op>.go     registerExpr(exprPrim{…})
//
// 一个元件的三份实现(静态检查 / 参考求值 / SQL)放在同一个文件里: 加元件 = 加一个文件,
// 几个人同时加不同的元件时不会改到同一个 switch。做法见 docs/adding-a-primitive.md。
//
// 登记在 init() 里(文件之间的顺序无关: 只是填 map;名字重复 → panic)。

import "fmt"

// stepPrim: 一个关系元件。三个钩子都必须有(scan 之外的步骤都接在前一步的结果上)。
type stepPrim struct {
	kind string
	// check: 静态检查(DRC)。读 / 改 c.cur(这一步之后的列),错误用 c.add 收集(不中断)
	check func(c *checker, st Step, at string)
	// eval: 参考求值器。读 / 改 e.schema、e.rows
	eval func(e *evaluator, st Step) error
	// sql: SQL 后端。lv = 前面的步骤累积的这一层(scan 时为 nil);返回之后的那一层(可以是包了一层子查询的新层)
	sql func(g *sqlGen, lv *sqlLevel, st Step) (*sqlLevel, error)
}

// exprPrim: 一个表达式元件。没有的钩子 = 这个后端不认识(和以前 switch 的 default 一样报错)。
type exprPrim struct {
	op     string
	typeOf func(p *Pipeline, e *Expr, cur Schema) (Type, error)
	eval   func(p *Pipeline, e *Expr, r Row, schema Schema, params Params) (any, error)
	// sql: pos = 这个表达式在 WHERE 的肯定位置(见 sqlGen.pos)。驱动在调用之前已经把 g.pos 设成了参数的位置
	// (keepPos 的元件: 不变;其他: false)
	sql func(g *sqlGen, e *Expr, lv *sqlLevel, pos bool) (string, error)
	// keepPos: 肯定位置穿过这个元件传给参数(只有 and / or)
	keepPos bool
	// nullRejects: 哪些列为 NULL 时这个条件一定不是 TRUE;all = 恒不为 TRUE(类型收窄用,check.go 的 narrow)。
	// nil = 不知道(保守: 不收窄)
	nullRejects func(p *Pipeline, e *Expr) (cols map[string]bool, all bool)
	// request: 值依赖请求(请求参数 / now)。逻辑表的来源不能用(checkSource)
	request bool
}

var (
	stepPrims = map[string]*stepPrim{}
	exprPrims = map[string]*exprPrim{}
)

func registerStep(p stepPrim) {
	if p.kind == "" || p.check == nil || p.eval == nil || p.sql == nil {
		panic(fmt.Sprintf("core: step primitive %q: kind / check / eval / sql 都必须有", p.kind))
	}
	if _, dup := stepPrims[p.kind]; dup {
		panic(fmt.Sprintf("core: step primitive %q registered twice", p.kind))
	}
	stepPrims[p.kind] = &p
}

func registerExpr(p exprPrim) {
	if p.op == "" {
		panic("core: expr primitive without op")
	}
	if _, dup := exprPrims[p.op]; dup {
		panic(fmt.Sprintf("core: expr primitive %q registered twice", p.op))
	}
	exprPrims[p.op] = &p
}

// checker: Check 的状态(一条流水线)。
type checker struct {
	p   *Pipeline
	i   int    // 当前步骤的下标(0 起)
	cur Schema // 当前步骤之后的列
	// gkeys: group 的键在当前位置的列名(跟着 project 的改名;被 project 丢掉 → "")。nil = 还没有 group
	gkeys   []string
	grouped bool
	add     func(format string, args ...any)
}

// evaluator: Eval 的状态(一条流水线)。
type evaluator struct {
	p       *Pipeline
	tables  map[string][]Row
	params  Params
	schema  Schema
	rows    []Row
	ordered bool // 最后有 sort: 行的顺序有意义
}

// rejectCol: nullRejects 的帮手: x 是列 → 记下。
func rejectCol(cols map[string]bool, x *Expr) {
	if x != nil && x.Op == "col" {
		cols[x.Name] = true
	}
}

// firstArgRejects: nullRejects 的帮手: 第一个参数为 NULL → 不是 TRUE。
func firstArgRejects(_ *Pipeline, e *Expr) (map[string]bool, bool) {
	cols := map[string]bool{}
	rejectCol(cols, e.Args[0])
	return cols, false
}

// paramArgType: in_list / starts_with / contains 的类型检查的共同部分: 参数 Name 的声明 + Args[0] 的类型。
func (p *Pipeline) paramArgType(e *Expr, cur Schema) (*ParamDecl, Type, error) {
	d := p.Param(e.Name)
	if d == nil {
		return nil, Type{}, fmt.Errorf("参数 %s 没有声明", e.Name)
	}
	a, err := p.typeOf(e.Args[0], cur)
	if err != nil {
		return nil, Type{}, err
	}
	return d, a, nil
}
