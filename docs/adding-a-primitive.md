# 加一个基本元件

核心代数(factory/core)的每个元件一个文件,三份实现(静态检查 / 参考求值 / SQL)放在一起,
在 `init()` 里登记到 `registry.go` 的登记表。共享的驱动(`Check` / `Eval` / `ToSQL` 的循环)只按名字查表,
加元件不用改驱动里的 switch —— 几个人同时加不同的元件时,冲突只在自己的文件和测试里。

新增元件仍然要单独评审(docs/design/core-algebra.md §4:封闭集合),先确认不能由已有元件组合(单元库)。

## 1. 加哪个文件

| 种类 | 文件 | 登记 | 例子 |
|---|---|---|---|
| 关系元件(流水线的一步) | `factory/core/prim_<kind>.go` | `registerStep(stepPrim{…})` | `prim_rank.go`(最小)、`prim_nest.go` |
| 表达式元件 | `factory/core/expr_<op>.go` | `registerExpr(exprPrim{…})` | `expr_contains.go`、`expr_ymd.go` |

文件开头写语义(NULL 规则、类型、SQL 的写法),再放构造函数(`func Rank(…) Step`)、`init()`、帮手函数。
名字重复登记会 panic;`registry_test.go` 检查每个表达式元件三份实现都有。

## 2. 钩子

**关系元件** `stepPrim`(三个都必须有):

| 钩子 | 做什么 | 状态 |
|---|---|---|
| `check(c *checker, st, at)` | DRC:改 `c.cur`(这一步之后的列),错误用 `c.add` 收集,不中断 | `c.i` 第几步、`c.gkeys` / `c.grouped`(group 相关) |
| `eval(e *evaluator, st)` | 参考求值:改 `e.schema` / `e.rows`(独立于 SQL,语义照文件开头的注释) | `e.tables` / `e.params`、`e.ordered` |
| `sql(g, lv, st) (*sqlLevel, error)` | 写进当前这一层,或 `g.wrap(lv, false)` 包一层子查询再写 | `lv.busy()` / `lv.joinBusy()` 决定要不要包 |

关联到别的表的元件(lookup / nest / child_agg 一类)用 `relation.go` 的共同部分:
`checkSource` / `related`(逻辑表)、`checkChild` / `childIndex` / `childGroup`(子表)、`lateral` / `childFrom`(LATERAL 子查询)。

**表达式元件** `exprPrim`(没有的钩子 = 那个后端报"不认识"):

| 钩子 / 标志 | 做什么 |
|---|---|
| `typeOf(p, e, cur)` | 类型和可空性(两个后端都依赖它) |
| `eval(p, e, r, schema, params)` | 参考实现(nil = NULL / UNKNOWN) |
| `sql(g, e, lv, pos)` | SQL 片段。参数用 `g.expr(…)`(驱动管 `g.pos`),请求参数用 `g.param(name)` |
| `nullRejects` | 可选:哪些列为 NULL 时一定不是 TRUE(类型收窄,`narrow`)。不写 = 不收窄(保守) |
| `request` | 值依赖请求(参数 / now):逻辑表的来源里不能用 |
| `keepPos` | 只有 and / or:WHERE 的肯定位置穿过它(`ymdCmp` 一类的改写靠它) |

## 3. 驱动里还剩的共享部分(要改时会和别人冲突)

- `core.go` 的 `Step` / `Expr` 结构体:新字段(例如新的 `Step.Xxx`)只能加在这里,还有 `Normalize`、`UsesNow` 的遍历。
- `check.go` 的 `Check` 末尾的跨步骤规则(scan 在第一步、sort 之后只接 limit、limit 之前要有 sort)。
- `sql.go` 的 `sqlLevel` 和 `render`:一层 SELECT 的形状(`group` / `rank` / `distinct` 字段和对应的写法)。
  需要新的 SELECT 子句的元件要加 `sqlLevel` 的字段和 `render` 的分支。
- `parse.go`:单元定义的语法(表达式元件的名字和参数个数),`Format`。
- `pushdown.go`:只认 pick_per_key / project / filter / lookup(不引用加的列的条件越过 lookup)。新元件不在这里 = 下推在它前面停下(安全)。`keyindex.go`:JSON 字段的等值条件另加和表达式索引同一个式子的条件(只给 SQL)。
- 工厂的其他地方:单元库 `factory/cells/standard.yaml`(词表)、`factory/compiler`(设计稿 → 流水线)、
  `factory/accept/diff*.go`(差分测试的数据生成;`diff.go` 里有一份和 `exprPrim.request` 一样的"依赖请求"列表)。

## 4. 测试(都要加)

1. **元件认证** `factory/core/cert_<name>_test.go`(或加到 `cert_test.go`):同一数据上 SQL(真的 Postgres)和参考求值器
   的结果逐条相同。覆盖 NULL、边界、文本的字节序、不合法的输入。`FACTORY_TEST_DB=… go test ./factory/core/...`
2. **检查** `check_<name>_test.go`:DRC 拒绝的每种写法各一条(错误信息里有 `at`)。
3. **差分测试** `factory/accept/diff*.go`:随机数据要能落到这个元件的边界上;想得到的"错误实现"做成敏感度目标
   (参考求值器认的开关,例如 `Expr.Loose`、`Step.NullEq` / `Step.Mutant`,SQL 后端和 IR 里没有),每个要被 ≥ 3 个请求区分。
4. **用到它的 API**:设计稿(`design/…`)→ `make design check prepare`,生成物入库。`make accept` 通过。
5. **变异余量** `scripts/mutate.sh`:往这个 API 的生成物(`gen/db/*.sql.go` / `internal/handler/**/_gen.go`)植入典型的 bug,
   `make margin` 每个变异 ≥ 3 个差分用例抓到。新写的差分用例的结果用 `diffResult(Result{…})` 创建(早停的计数,
   `factory/accept/early.go`;漏了会在验收时报"早停的计数不一致")。随机数据只从 `rnd` 取,遍历 map 时先排序键
   (同一个种子要造出同样的用例)。

全部闸门:`make check`、`make test`、`FACTORY_TEST_DB=… go test ./factory/...`、`make accept`、`scripts/projects.sh`、
`make margin`、`scripts/ci/migrations-check.sh`。
