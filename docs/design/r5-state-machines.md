# 设计稿：R5 — 状态机（storefront v2 的订单 saga）

> 版本 v0.1（2026-11-22）· 状态：**评审中**（标 ⚖️ 的地方需要决定）
> 上级计划：`docs/design/storefront-v2-readiness.md` 的 R5（C6 状态机 → W5、M4–M9、J2）。**95% 的过关线 = R5 完成（27 / 28）。**
> 前提：R3（写操作是数据、一个事务、乐观锁、状态差分）和 R4（`docs/design/r4-events-messages.md`：outbox、消息处理器、去重、可重入 DRC、不承诺顺序）。R2-3 的测试时钟（`FACTORY_TEST_CLOCK` / `X-Factory-Now`）。

---

## 0. 定位

- storefront 的下单流程是**编排式 saga**（storefront 设计 §6）：order 服务按状态机发命令、收结果、必要时补偿。
- 用 R4 的消息处理器也能把 M4–M9 写成 6 个独立的处理器，每个自己写"检查当前状态 → 改状态 → 发消息"。问题是：**状态图只存在于 6 份设计稿的条件里**，没有地方检查"每个订单都能走到终态"、"补偿是否完整"、"超时是否覆盖了每个等待状态"。
- R5 把状态图本身变成设计稿里的**一个声明**：工厂从它生成 6 个转移处理器 + 1 个超时任务 + 下单接口的初始转移，并且以这个声明为参考模型做基于模型的测试。
- 最大的风险是做成工作流引擎（§8）。本稿只做 saga 需要的最小集合，**清单之外一律不做**。

---

## 1. 最小集合（做什么、不做什么）

| 做 | 说明 |
|---|---|
| 有限个**状态**，其中若干个**终态** | 状态存成实体行上的一个枚举列（§3.1） |
| **转移** = 当前状态 + 收到的消息 → 守卫 → 操作 → 新状态 + 要发的消息 | 触发只有两种：消息（R4 处理器）、超时 |
| **初始转移**：一个 HTTP 写接口创建实体并进入初始状态 | W5 下单 |
| **超时**：在等待状态停留太久 → 按固定间隔重发，或在期限之后进入补偿 | J2 |
| 迟到 / 重复消息的处理按状态图**自动分类**（§4.4） | 不需要逐个声明 |

| 不做 | 为什么 |
|---|---|
| 并行分支（同时等两个结果，再汇合） | saga 是串行的（先预留再扣款）。并行需要"多个状态列 + 汇合的语义 + 部分失败的补偿顺序"，复杂度翻倍而没有需求；需要并行时让两个服务各自独立回答、由一个状态等两个事件，属于新设计 |
| 子状态机 / 层次状态 | 只有一个 saga；嵌套会让"实体在什么状态"不再是一个列的值，查询（Q7）和指标都变复杂 |
| 历史状态、动态生成的状态机、人工审批步骤 | 没有需求；人工步骤是另一种入口（HTTP 触发的转移），先不开 |
| 跨实体的状态机（一个转移改两个订单） | 一个转移 = 一个实体的一个事务（§3.3） |
| 长时间的计时（天、周）、日历 | 超时以秒 / 分钟计，由一个扫描任务驱动；长计时是调度器的事 |

---

## 2. 设计语言

```yaml
状态机: OrderSaga
实体: orders                     # 状态存在这张表的行上
键: id
状态列: status                   # 枚举 OrderStatus
进入时间列: updated_at           # 进入当前状态的时刻,超时的基准(只有转移能写它,DRC)
版本列: version                  # 乐观锁(§3.3)
重发计数列: state_attempts       # 进入新状态时清零

开始:                             # 初始转移(W5 下单,HTTP)
  API: PlaceOrder
  操作:
    - 订单: { 新建: orders, 值: { customer_id: 输入.customer_id, total: 计算.total, status: RESERVING } }
    - 明细: { 新建: order_items, 每项: 输入.items, 值: { order_id: 订单.id, line_no: 项.序号, sku: 项.sku, qty: 项.qty, price: 项.price } }
  发出: [ order.created, inventory.reserve: { order_id: 订单.id, items: 输入.items } ]

状态:
  RESERVING:  { 超时: 30 秒, 超时后: { 重发: inventory.reserve }, 期限: 15 分钟, 期限后: → RELEASING }
  CHARGING:   { 超时: 30 秒, 超时后: { 重发: payment.charge } }            # 没有期限:见 §4.3
  CONFIRMING: { 超时: 30 秒, 超时后: { 重发: inventory.commit } }
  RELEASING:  { 超时: 30 秒, 超时后: { 重发: inventory.release } }
  CONFIRMED:  { 终态: 是 }
  CANCELLED:  { 终态: 是 }

载入:                             # 转移要发的消息需要明细时,在同一个事务里读(R2 的 nest,有上限)
  items: { 表: order_items, 条件: [id = order_id], 排序: [line_no 小到大], 上限: 50, 列: [sku, qty] }

转移:
  - { 从: RESERVING,  收到: inventory.reserved,            到: CHARGING,   发出: [ payment.charge: { order_id: 键, amount: 实体.total } ] }
  - { 从: RESERVING,  收到: inventory.reservation_failed,  到: CANCELLED,  发出: [ order.cancelled: { order_id: 键, reason: 消息.reason } ] }
  - { 从: CHARGING,   收到: payment.succeeded, 守卫: "消息.amount = 实体.total",
                                                            到: CONFIRMING, 发出: [ inventory.commit:  { order_id: 键, items: 载入.items } ] }
  - { 从: CHARGING,   收到: payment.failed,                 到: RELEASING,  发出: [ inventory.release: { order_id: 键, items: 载入.items } ] }
  - { 从: CONFIRMING, 收到: inventory.committed,            到: CONFIRMED,  发出: [ order.confirmed: { order_id: 键 } ] }
  - { 从: RELEASING,  收到: inventory.released,             到: CANCELLED,  发出: [ order.cancelled: { order_id: 键, reason: "PAYMENT_FAILED" } ] }
```

- **重发 / 补偿时的消息内容**：`重发: inventory.reserve` 用"进入这个状态的那个转移"声明的同一个消息模板重新求值（新 id，R4 E8）；`期限后: → RELEASING` 使用指向 RELEASING 的转移模板（发 `inventory.release`）。
- **守卫**：可选的谓词（R1 的谓词元件，作用在实体行 + 消息上）。守卫为假 = 这条消息在这个状态下**被拒绝**（§4.4），不转移。
- **转移上的操作**：除了改状态，还可以有 R3 的写操作（例：记一条状态历史），和改状态在同一个事务里。订单 saga 不需要。
- 具体的键名实现时以设计语言的既有风格为准；`docs/design-language.md` 由注册表生成。

### 2.1 和 storefront 设计 §6.2 的差别（要回写到 storefront 设计书）

| 差别 | 原因 |
|---|---|
| 没有 PENDING：下单直接写成 RESERVING ⚖️ S3 | PENDING → RESERVING 没有等待的东西（"发 reserve"就在下单的事务里写进 outbox）。持久化 PENDING 还需要另一个触发者把它推进 |
| 没有 `saga_state` 表：状态在 `orders` 行上 ⚖️ S1 | 一行一个事务、Q7（卡住的订单）直接查 orders、不需要两表一致 |
| RESERVING 有 15 分钟的期限，超过 → RELEASING | J2 的"进入补偿"（§4.3） |
| 增加 `inventory.committed` / `inventory.released` 的契约 | 图里有、清单里缺（R4 §5.3） |

---

## 3. 生成的代码

### 3.1 实体表的要求（DRC）

实体表必须有：键、状态列（枚举，整数列）、进入时间列（timestamptz）、版本列（整数）、重发计数列（整数）。order 服务的表结构（R5-1 写进样例 `examples/storefront-v2/order/db/schema/`）：

```sql
CREATE TABLE orders (
    id             BIGSERIAL   PRIMARY KEY,
    customer_id    TEXT        NOT NULL,
    total          NUMERIC     NOT NULL CHECK (total >= 0),
    status         INTEGER     NOT NULL CHECK (status BETWEEN 1 AND 6),  -- 1 RESERVING 2 CHARGING 3 CONFIRMING 4 RELEASING 5 CONFIRMED 6 CANCELLED
    version        INTEGER     NOT NULL DEFAULT 1,
    state_attempts INTEGER     NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL
);
CREATE INDEX orders_waiting_idx ON orders (status, updated_at) WHERE status NOT IN (5, 6);  -- 超时扫描 + Q7
```

DRC：项目里其他的写接口**不能写**状态列、进入时间列、重发计数列（只有状态机的转移能写），否则超时的基准会被打乱。

### 3.2 一个转移 = 一个 R4 处理器

工厂把每个（消息类型）生成一个 R4 消息处理器（M4–M9 各一个，队列 `order.<消息类型>`），处理器的操作是：

```
BEGIN; 去重(R4 §4.2)
  读实体行(status, version, …) + 载入(若这个消息的转移需要)
  按 (status, 消息类型) 查生成的转移表:
    有转移且守卫为真 → UPDATE orders SET status = 到, updated_at = now(), state_attempts = 0, version = version + 1
                         WHERE id = $k AND version = $v              ← 乐观锁
                       + 转移上的操作 + 写 outbox(发出的消息)
    其他               → 按 §4.4 分类(忽略 / 拒绝),不写实体
COMMIT; ack
```

- `now()` 是平台时钟（R2-3），测试时固定 → `updated_at` 可复现。
- 生成的转移表是常量（`map[[2]string]transition`），**不是**解释执行的 YAML：和工厂其他部分一样，确定性生成、可审查。

### 3.3 同一实体的并发：乐观锁 ⚖️ S2

同一个订单可能同时有两件事：回答到达 + 超时扫描触发重发；或者同一个回答的两次投递（不同 id）。规则：
- 读行时记下 `version`；更新带 `AND version = $v`；**0 行 = 冲突** → 回滚整个事务，在进程内**重新执行**（重新读状态、重新判断），最多 3 次；仍冲突 → R4 的暂时性错误（重试队列）。
- 重新执行时状态可能已经变了：超时扫描输给了回答 → 重新读到 CHARGING → 不再重发；回答输给了重发 → 重新读到仍是 RESERVING（version 变了，attempts + 1）→ 照常转移。
- 不用 `SELECT … FOR UPDATE`：一个订单同一时刻通常只有一条消息在路上，冲突罕见；乐观锁和 R3 的 `update + 版本列` 是同一个元件，不新增执行路径。

### 3.4 超时：一个扫描任务（J2）

每个服务进程里一个超时协程，每个数据库同时只有一个在工作（advisory lock，同 R4 的中继）⚖️ S8，每 5 秒一轮：

```sql
SELECT id, status, version, state_attempts FROM orders
 WHERE status = ANY($waiting) AND updated_at < $now - $timeout(status, state_attempts)
 ORDER BY updated_at LIMIT 100
```

- 每个命中的实体在**自己的事务**里执行超时转移（乐观锁同 §3.3）：
  - **重发**：`state_attempts + 1, version + 1`，`updated_at` 不变（期限按进入状态的时刻算），发出重发的消息。第 n 次重发发生在进入状态之后 30s + 60s × (n − 1)：首次 30 秒，之后每 60 秒一次（`$timeout(status, attempts)` 即这个式子）。
  - **期限后**：普通转移到指定状态（`state_attempts = 0`、`updated_at = now()`），发出该转移的消息。
- 超时的判断用 `$now`（平台时钟），测试里由测试驱动器推进；不用数据库的 `now()`。
- 一轮最多 100 个：积压时分多轮，保证一轮的事务短；`factory_sm_timeout_backlog` 指标反映没处理完的个数。

### 3.5 初始转移（W5）

`开始:` 生成一个 HTTP 写接口（R3 的组合命令 + R4 的 `每项` + `发出`）：订单 + N 个明细 + 两条消息，一个事务。`计算.total` 是业务规则的纯函数（明细的数量 × 单价之和，十进制精确）。单价从哪里来（web 从 catalog 取了传进来）是 web 的组合逻辑，不在工厂的范围。

---

## 4. 语义

### 4.1 转移的确定性（DRC）

- 同一个 `(从, 收到)` 最多一条无守卫的转移；有多条时守卫必须互斥（工厂只接受 `守卫` + `否则` 的形式，不做谓词的可满足性判断）。
- 终态没有出边（不能有 `从: CONFIRMED`）。
- 每个非终态：从初始状态可达；**能到达某个终态**；有超时（没有超时的等待状态在依赖丢消息时会永远卡住）。
- 每个 `收到` 的消息类型在契约里存在，且 order 服务订阅了它；每个 `发出` 的消息存在。

### 4.2 超时的语义

| 状态 | 超时 | 期限 | 理由 |
|---|---|---|---|
| RESERVING | 30s 后重发，之后每 60s | 15 分钟 → RELEASING | 还没扣款，补偿是安全的：release 是可重入的，并且会留下墓碑挡住迟到的预留（R4 §5.3） |
| CHARGING | 同上 | **无** | 没有"撤销扣款"的命令：不知道支付结果就补偿，可能出现"扣了款、订单却取消"。重发 `payment.charge` 是幂等的（同订单号返回第一次的结果），支付恢复后一定能拿到结果 |
| CONFIRMING / RELEASING | 同上 | 无 | 命令可重入，重发直到 inventory 回答 |

⚖️ S4：期限只给 RESERVING。storefront 的故障实验"inventory / payment 宕机 10 分钟，恢复后全部继续推进"要求停机期间**不**取消订单：RESERVING 的期限（15 分钟）长于这个实验。

### 4.3 有界性（"5 分钟内到达终态"的工程含义）

依赖全部恢复的时刻记为 t_r。之后每个实体最多还要走 3 步（RESERVING → CHARGING → CONFIRMING → CONFIRMED，或经 RELEASING 到 CANCELLED），每一步最慢 = 一个重发周期（≤ 60s）+ 处理时间。所以**恢复后 ≤ 4 分钟到达终态**（3 × 60s + 余量），满足 storefront 的"恢复后 5 分钟内"。这个上界在测试里用测试时钟验证（§5.3），并作为指标告警的阈值来源（§6）。

### 4.4 不能转移的消息：按状态图自动分类 ⚖️ S5

| 分类 | 判定（由工厂从状态图预先算出） | 处理 |
|---|---|---|
| **迟到 / 重复** | 消息类型能在某个**先于**当前状态的状态上触发转移（当前状态从那个状态可达）。例：CONFIRMING 收到 `inventory.reserved`（重发引起的第二个回答） | 忽略：提交去重记录，ack，计 `ignored{reason=late}` |
| **守卫为假** | 有转移但守卫不成立。例：支付金额 ≠ 订单金额 | 拒绝：发到 DLQ（带原因）+ 告警。这是数据不一致，不应自动吞掉 |
| **不可能** | 这个消息类型在当前状态及其之前的任何状态上都没有转移。例：RESERVING 收到 `inventory.committed` | 拒绝：DLQ + 告警（说明某处有 bug） |
| **实体不存在** | 键找不到 | 拒绝：DLQ（R3 的 `NOT_FOUND` 在消息里不重试） |

终态上的迟到消息同样是"忽略"。**迟到消息不触发补偿**：迟到的预留由 inventory 的墓碑挡住（R4 §5.3）⚖️ S6，状态机保持"终态不再动"。

---

## 5. 验证

### 5.1 参考模型 = 状态机的声明本身

参考求值器是一个**独立实现的解释器**：读 IR 里的状态机，在模型状态（R3）上执行转移 / 超时 / 分类，消息日志和已处理集合沿用 R4 §7.1。生成的代码（转移表 + 处理器 + 超时任务）和解释器对同一串输入比较结果 —— 和查询工厂"SQL 后端 vs 参考求值器"是同一个结构。

### 5.2 基于模型的测试

| 测试 | 内容 |
|---|---|
| **转移全覆盖** | 对每个（状态 × 每个订阅的消息类型）构造一次投递：期待的结果是"转移到 X"、"忽略"或"拒绝"，和解释器比较。6 状态 × 6 消息 = 36 个格子全部出现 |
| **守卫** | 每个守卫的真和假都出现 |
| **随机游走** | 从下单开始，随机选择：投递一个期待的回答 / 投递重复（同 id）/ 投递重发引起的回答（新 id）/ 投递迟到的旧回答 / 推进时钟并跑一轮超时。每步比较订单表、outbox、去重表 |
| **并发** | 同一个订单的回答和超时扫描同时执行；同一回答的两份（不同 id）同时执行（真实并发，goroutine × 真实 Postgres）。期待：只发生一次转移，结果与某个串行顺序相同 |
| **超时** | 用测试时钟：每个等待状态的"刚好未到 / 刚好到"（`<` 和 `≤`）、第 1 / 2 / n 次重发的时刻、RESERVING 期限前后 |

### 5.3 端到端的环境模型：依赖的停机和恢复

把 inventory（M1–M3）、payment（M10）、order（M4–M9、W5、J2）三个服务的生成代码放进**一个测试驱动器**（进程内，各自的数据库），驱动器扮演 RabbitMQ：可以让某个服务"停机"（消息积压不投递）、丢弃 / 重复 / 乱序投递、推进时钟。场景：

| 场景 | 期待 |
|---|---|
| 正常路径 ×N（库存足 / 不足、支付成功 / 失败） | 每个订单到达对应的终态 |
| inventory 停 10 分钟，然后恢复 | 停机期间订单停在 RESERVING，不取消；恢复后 ≤ 4 分钟全部到达终态 |
| payment 停 10 分钟，然后恢复 | 停在 CHARGING；恢复后 ≤ 4 分钟到达终态；每个订单 `payments` 最多一行 |
| inventory 停 20 分钟（超过 RESERVING 的期限） | 订单经 RELEASING 取消；恢复后迟到的预留被墓碑挡住，库存不泄漏 |
| 每一步都重复投递 / 乱序 | 同上的终态，无重复扣款 |

### 5.4 不变量

| # | 不变量 |
|---|---|
| S-I1 | **终态可达**：依赖恢复后，每个订单在 §4.3 的上界内到达 CONFIRMED 或 CANCELLED |
| S-I2 | **补偿完整**：静止时（没有在途消息），每个 CANCELLED 的订单在 inventory 里没有 HELD 的预留；`stock` 的守恒式（R4 I7）成立 |
| S-I3 | **扣款最多一次**：每个订单 `payments` ≤ 1 行；CONFIRMED ⇔ 支付成功；支付成功的订单不会是 CANCELLED（CHARGING 没有期限保证了这一点） |
| S-I4 | 终态不再变化；状态的序列是状态图里的一条路径（记录每个订单的状态历史，逐步检查） |
| S-I5 | 每个订单在任意时刻最多一条"本状态的命令"在路上（不算重发） |

### 5.5 变异余量

每个变异 ≥3 个用例抓到：

| 变异 | 应该抓到它的 |
|---|---|
| 转移的"到"换成别的状态 | 转移全覆盖 |
| 去掉守卫 | 守卫为假的用例 |
| 去掉 `AND version = $v` | 并发（双重转移 → 发出两条命令） |
| 重发不累加 / 进入新状态不清零 `state_attempts` | 超时的重发间隔用例 |
| 超时比较 `<` ↔ `≤` | 测试时钟的"刚好到"用例 |
| 期限给了 CHARGING | payment 停机场景（S-I3） |
| 迟到消息被重新应用 / 迟到当作"不可能"送进 DLQ | 随机游走的迟到投递 |
| 终态接受转移 | S-I4 |
| 进入终态时不发 `order.confirmed` / `order.cancelled` | 消息日志的差分 |
| RELEASING 的转移发错命令（发 commit） | S-I2 |

---

## 6. 运行时（api-factory-runtime v0.5.0）和指标

- 新增：转移执行器（读行 → 查表 → 乐观更新 → 冲突重试）、超时扫描（advisory lock、批量、重发时刻的计算）、测试时钟对超时任务生效（测试驱动器可以直接调用"跑一轮超时(now)"）。
- 指标：

| 指标 | 用途 |
|---|---|
| `factory_sm_transitions_total{machine, from, to, trigger}` | 每条边的流量；trigger = 消息类型 / timeout / deadline |
| `factory_sm_messages_ignored_total{reason}` / `_rejected_total{reason}` | 迟到的多少反映重发是否过于激进；rejected > 0 → 告警 |
| `factory_sm_oldest_in_state_seconds{state}` | 超时扫描顺带算出。**"订单 5 分钟内到达终态"的 SLO 数据源**（storefront 设计 §6.4 的"最老的卡住时长"）；> 4 分钟（§4.3 的上界）→ 告警 |
| `factory_sm_instances{state}` | 每个状态的实体数（"卡住的订单数"） |
| `factory_sm_timeout_backlog` | 一轮没扫完的个数 |

"下单到 CONFIRMED 的耗时"直方图（storefront 的 60 秒用户旅程 SLO）：进入终态的转移记录 `now() − created_at`，由生成代码导出。

---

## 7. 映射：27 / 28

| # | API | 由什么生成 | 还要人写的 |
|---|---|---|---|
| W5 | 下单 | `开始:`（R3 组合命令 + R4 `每项` / `发出`） | `total` 的纯函数（AI，闸门内） |
| M4 | `inventory.reserved` | 转移 RESERVING → CHARGING | — |
| M5 | `inventory.reservation_failed` | 转移 RESERVING → CANCELLED | — |
| M6 | `payment.succeeded` | 转移 CHARGING → CONFIRMING（守卫：金额） | — |
| M7 | `payment.failed` | 转移 CHARGING → RELEASING | — |
| M8 | `inventory.committed` | 转移 CONFIRMING → CONFIRMED | — |
| M9 | `inventory.released` | 转移 RELEASING → CANCELLED | — |
| J2 | 超时处理 | `状态:` 的超时 / 期限 | — |

- 加上 R1–R4 的 19 个 → **27 / 28（96%）**，过关。
- 剩下的 1 个：**J1**（释放过期的预留，按条件批量更新）→ R6，或者作为"手写的 5%"。本稿把预留的过期时间定为 24 小时（只当安全网），J1 晚做不影响 saga 的正确性。
- 不计入分母、仍然手写的：web 的组合（价格、是否有货）、退款这类"外"的流程、DLQ 的人工处理。

### 7.1 中立样例（不只为 storefront 成形）

工厂的 `design/` 里加一个与 storefront 无关的状态机：**任务审批**（DRAFT → REVIEWING：发 `review.request`；收到 `review.approved` → APPROVED / `review.rejected` → REJECTED；REVIEWING 超时重发、期限后 → EXPIRED）。它用到同样的元素（初始转移、两个终态 + 一个期限终态、守卫、超时），但没有补偿 —— 检验状态机不依赖 saga 的特殊结构。

---

## 8. 风险：变成工作流引擎

| # | 风险 | 应对 |
|---|---|---|
| 1 | **一点点加功能**，最后变成通用的工作流引擎（并行、子流程、人工步骤、可视化编辑器……） | §1 的"不做"清单写死；任何新元素按核心代数的规矩走"新元件 + 单独评审 + 两个以上需求"。指标：状态机相关的设计语言关键字数（目标 ≤ 12）、运行时 `sm` 包的代码量，每期记录 |
| 2 | **在途实例和状态机的变更**：改了状态机，旧状态里的订单怎么办 | 生成 `machines.lock.yaml`：新增状态 / 转移可以；删除或改名一个状态 → DRC 要求部署前检查"该状态的实例数 = 0"（生成一个部署前的检查查询，release 流程里执行）⚖️ S7 |
| 3 | 超时扫描和中继、消费循环抢数据库连接 | 三者共用一个连接池，但各有上限（扫描 ≤ 2 个连接）；扫描每轮 ≤ 100 个实体 |
| 4 | CHARGING 没有期限：支付长期不可用时订单一直挂着 | 这是有意的（§4.2），由 `oldest_in_state_seconds{state="CHARGING"}` 告警交给人；"撤销扣款"的命令出现时再加期限 |
| 5 | 守卫写成复杂的业务判断，状态机变成业务逻辑的容器 | 守卫只能用谓词元件（不能调用纯函数）；需要计算的判断放进发送方的消息内容里 |

**明确不做（记进 `docs/memo/out-of-scope.md`）**：并行分支和汇合、子状态机、历史状态、人工步骤（HTTP 触发的中间转移）、跨实体转移、以天计的计时、协同式（choreography）的"状态机"（通知类的订阅用 R4 的处理器）、按 subject 有序的消费（R4 E4）。

---

## 9. 实施顺序

| 步 | 内容 | storefront |
|---|---|---|
| R5-1 | 状态机的设计语言 + DRC（§4.1）+ 解释器（参考模型）+ 生成转移处理器 + 乐观锁；order 样例项目（表结构、契约）；转移全覆盖 + 随机游走 + 并发 | W5、M4–M9 |
| R5-2 | 超时扫描（重发间隔、期限）+ 测试时钟 + 环境模型（三服务的停机 / 恢复场景）+ 不变量 S-I1–S-I5 + 变异余量 + 指标 + `machines.lock.yaml` | J2 |

完成后 storefront 27 / 28 → **readiness 的过关线**。之后按 readiness §5 的决定开始 storefront v2 的开发（R6 可以并行或放弃）。

---

## ⚖️ 需要决定

| # | 问题 | 推荐 |
|---|---|---|
| S1 | saga 的状态存在哪 | 实体行上（`orders.status / updated_at / version / state_attempts`），不要 storefront 设计里的 `saga_state` 表（一个事务、Q7 直接查、没有两表一致的问题） |
| S2 | 同一实体的并发 | 乐观锁（版本列，冲突时进程内重新执行 ≤3 次）；不用 `SELECT … FOR UPDATE` |
| S3 | PENDING 状态 | 不持久化：下单的事务直接写 RESERVING 并把 `inventory.reserve` 写进 outbox |
| S4 | 超时策略 | 所有等待状态超时重发（首次 30s，之后每 60s）；只有 RESERVING 有期限（15 分钟 → RELEASING）；CHARGING 不自动补偿（没有撤销扣款的命令） |
| S5 | 不能转移的消息 | 按状态图自动分类：迟到 / 重复 → 忽略并计数；守卫为假、不可能的消息、实体不存在 → DLQ + 告警 |
| S6 | 取消之后迟到的预留 | 在 inventory 一侧用墓碑挡住（R4 §5.3），状态机的终态保持不动；不在状态机里加"终态的补偿" |
| S7 | 状态机变更与在途实例 | 锁文件；只允许新增，删除 / 改名状态要求部署前该状态实例数 = 0 |
| S8 | 超时任务在哪里跑 | 服务进程内的协程（advisory lock 选主，每 5 秒一轮），不用 Kubernetes CronJob（分钟级粒度不够、多一个部署物） |
