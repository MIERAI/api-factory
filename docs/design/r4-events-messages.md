# 设计稿：R4 — 事件（outbox + RabbitMQ）与消息处理器

> 版本 v0.3（2026-11-22）· 状态：**已实现**（⚖️ 全部：2026-11-22 用户决定**按推荐**;实现和设计稿的差别见文末"实现记录"）
> 上级计划：`docs/design/storefront-v2-readiness.md` 的 R4（C4 事件 + C5 消息处理器 → M1–M3、M10–M12，以及 W5 的事件）。
> 前提：R3 命令工厂（`docs/design/r3-command-factory.md`）：写操作是数据、平台在一个事务里执行、错误映射、状态差分测试、故障注入、并发不变量、重放幂等。本稿只在它上面加东西，不改它的语义。
> R5（状态机）在本稿的消息处理器之上扩展。

---

## 0. 定位

- R3 的写操作只有一个入口：HTTP 请求。R4 加两件事：
  1. **写操作可以发消息**（事件 / 命令）：消息和写操作在**同一个事务**里落进 outbox，由平台的中继发到 RabbitMQ。
  2. **消息可以触发写操作**：消息处理器 = 入口换成 RabbitMQ 的 R3 命令。去重、确认、重试、死信由平台统一做。
- 这两件事都不需要 AI 写任何 IO 代码：消息的内容是**列 / 值的映射**（和 R3 的写入值同一套表达式），中继和消费循环在运行时里。
- 边界：`boundaries.md` 的"外"里有"流式 / 异步 / 推送"。本稿只把其中的**一小块**移进"内"：*至少一次投递 + 幂等消费*的点对点消息。推送给浏览器、流式响应、按顺序消费的流（Kafka 式的分区日志）仍然是外。
- 验收标准：storefront v2 的 M1–M3（库存预留 / 确认 / 释放）、M10（支付）、M11–M12（通知），以及 W5 下单时发出的 `order.created` / `inventory.reserve`（W5 整体在 R5 计入）。

**通用性的检查**：试用（一家公司的参照 API）全部是只读查询，用不上 R4。和 R3 一样，每种形状在工厂自己的 `design/` 里写一个与 storefront 无关的样例（§9），不让消息处理只为一个使用者成形。

---

## 1. 原则

1. **消息也是数据。** 写操作声明它发什么消息、每个字段取什么值；平台负责序列化、写 outbox、发送。AI 写的业务规则纯函数照旧只返回值。
2. **至少一次，靠幂等变成"效果恰好一次"。** 投递层只保证至少一次（中继会重发、RabbitMQ 会重投）。正确性由两层幂等保证：**传输层去重**（同一条消息的 id）+ **语义层幂等**（同样内容的新消息，靠 R3 的条件更新 / 幂等新建，§5.4）。
3. **不承诺顺序。** 平台不保证任何消息之间的先后（§3.4）。处理器必须对乱序安全；需要顺序的地方由状态（R3 的条件、R5 的状态机）来判断，不靠投递顺序。
4. **先提交，再确认。** 消费端只在数据库事务提交之后才 ack；发送端只在 RabbitMQ 发布确认（publisher confirm）之后才把 outbox 标为已发送。

---

## 2. 设计语言：消息的声明

### 2.1 消息类型（契约）属于生产者

每个服务的 `design/<领域>/消息.yaml` 声明**它发出的**消息类型：

```yaml
消息:
  inventory.reserved:
    种类: 事件                     # 事件 = 广播"发生了什么";命令 = 让某个服务做某件事
    版本: 1
    主体: order_id                 # envelope.subject 取哪个字段(实体的键)
    字段:
      order_id: { 类型: 整数64, 说明: 订单号 }
      items:    { 类型: 列表, 个数: "1~50", 元素: { sku: 文本, qty: 整数32 } }
  inventory.reserve:               # 命令由"执行它的服务"声明(inventory 拥有这个契约,order 只是发送方)
    种类: 命令
    版本: 1
    主体: order_id
    字段: { … }
```

- 命令的契约归**接收方**（它定义自己接受什么），事件的契约归**发送方**。这样每个契约只有一个拥有者。
- 类型复用现有的类型注册表（整数 / 十进制 / 文本 / 时间 / 日期 / 枚举），新增**列表**（元素为平铺的字段，必须有个数上限，和 R2 `nest` 的"必须有上限"同一个规矩）。

### 2.2 写操作声明它发的消息

R3 的设计稿加一节 `发出:`，值的表达式和 R3 写入值是同一套（输入 / 常量 / 前面操作的返回值 / `now()`）：

```yaml
API: CreateTask              # 中立样例(§9)
种类: 新建
数据来源: tasks
发出:
  - task.created: { task_id: 新建.id, title: 输入.title, created_at: 新建.created_at }
```

- 发出的消息在**操作全部成功**时才写入 outbox；任何操作失败 → 事务回滚 → 没有消息（R3 §5.3 的故障注入同时检查这一点）。
- 一个写操作可以发多条（W5：`order.created` + `inventory.reserve`）。

### 2.3 envelope（所有消息共用的外层，和 storefront 设计 §5.3 一致）

| 字段 | 取值 | 由谁定 |
|---|---|---|
| `id` | UUIDv7 的文本 ⚖️ E2 | 平台，在事务里生成并写进 outbox（重发时不变） |
| `type` / `version` | 消息类型 / 契约版本 | 设计稿 |
| `occurred_at` | 事务的 `now()`（R2-3 的平台时钟，测试时固定） | 平台 |
| `source` | 服务名（项目 `factory.yaml` 的 proto 包名前缀） | 平台 |
| `subject` | 契约声明的主体字段的值（文本） | 设计稿 |
| `traceparent` | 当前请求 / 当前处理的 span（W3C） | 平台 |
| `data` | 字段 → 值 | 设计稿 |

数值的表示和查询工厂的 API 表示一致：十进制 → 字符串，时间 → UTC RFC3339，日期 → `YYYY-MM-DD`（不经 JSON 数字，避免精度丢失）。

### 2.4 契约文件和版本规则

- 工厂从 `消息.yaml` 生成 `contracts/events/<type>.v<N>.json`（JSON Schema 2020-12，envelope + data），放在**系统级的共享目录**（storefront 仓库根的 `contracts/`；工厂样例是 `examples/storefront-v2/contracts/`），项目的 `factory.yaml` 用 `contracts: ../contracts` 指向它 ⚖️ E7。
- 消费方的处理器从契约文件读字段和类型（解码代码由工厂生成），不再自己声明一遍。
- **版本规则（DRC + 锁文件）**：沿用 `proto.lock.yaml` 的做法，生成 `contracts.lock.yaml`。同一个版本里**只能新增可选字段**；删字段、改类型、改含义、把可选改成必填 → 编译失败，提示"发布新版本 `vN+1`"。新旧版本并行期间，生产者两个版本都发（设计稿写 `版本: [1, 2]`），由使用方全部迁走后再删。
- 消费者**忽略不认识的字段**（生成的解码器不开 `additionalProperties: false` 校验 data 的多余键），但缺少必填字段 / 类型不对 = 毒消息（§4.4）。

---

## 3. Outbox 和中继

### 3.1 表（平台表，`factory_` 前缀）

```sql
CREATE TABLE factory_outbox (
    seq          BIGSERIAL   PRIMARY KEY,
    id           UUID        NOT NULL UNIQUE,      -- envelope.id
    kind         SMALLINT    NOT NULL,             -- 1 event, 2 command
    type         TEXT        NOT NULL,
    version      INTEGER     NOT NULL,
    subject      TEXT        NOT NULL,
    envelope     JSONB       NOT NULL,             -- 完整的消息(发送时原样发出)
    occurred_at  TIMESTAMPTZ NOT NULL,
    published_at TIMESTAMPTZ,                      -- NULL = 还没发出(确认过)
    attempts     INTEGER     NOT NULL DEFAULT 0,
    last_error   TEXT
);
CREATE INDEX factory_outbox_pending_idx ON factory_outbox (seq) WHERE published_at IS NULL;

CREATE TABLE factory_processed_messages (       -- 消费端去重(§4.2)
    handler      TEXT        NOT NULL,
    message_id   UUID        NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (handler, message_id)
);
```

- 这两张表由工厂生成到项目的 `db/schema/zz_factory_platform.sql`（确定性生成，走项目既有的迁移流程，评审时看得见）⚖️ E1。

### 3.2 写入：和 R3 的操作同一个事务

R3 的事务执行器在执行完全部操作、**提交之前**，按设计稿的顺序 `INSERT INTO factory_outbox`。任何一步失败都回滚，outbox 里不会有"没有发生的事"的消息。提交后通知中继（`NOTIFY factory_outbox`，只是唤醒，丢了也没关系：中继另有 1 秒的轮询兜底）。

### 3.3 中继：发布确认之后才标记

每个服务进程里一个中继协程；**每个数据库同时只有一个在工作**（`pg_try_advisory_lock`，拿不到锁的副本待命）⚖️ E3。循环：

1. `SELECT … FROM factory_outbox WHERE published_at IS NULL ORDER BY seq LIMIT 100`（不加行锁，不在事务里做网络 IO）
2. 在一个开了 confirm 模式的 channel 上按 seq 顺序发布（持久化消息，`message_id` = envelope.id，`type`、`traceparent` 也放进 AMQP 属性）
3. 等这一批的 confirm：ack 的 → `UPDATE … SET published_at = now()`；nack / 超时 / 连接断开 → `attempts + 1, last_error`，退避（1s → 30s 封顶）后重来
4. 已发送的行保留 7 天后分批删除（每次 ≤1000 行）

**崩溃 / 重启的语义**：

| 崩在哪里 | 结果 |
|---|---|
| 发布之前 | 什么都没发生，重启后照常发 |
| 发布之后、confirm 之前 / confirm 之后、标记之前 | 重启后**同一条消息（同一个 id）再发一次** → 下游靠 id 去重（§4.2） |
| 标记之后 | 不会重发 |

所以 outbox 的保证是：**已提交的消息最终至少发出一次，id 不变**；"最终"的前提是 RabbitMQ 恢复，期间的积压看 §6 的指标。

**路由不到**：命令发布时开 `mandatory`：没有队列接收 = 配置错误 → 当作发布失败留在 outbox 里并告警（命令绝不能静默消失）。事件不开 `mandatory`：没有订阅者是正常的（例如 `order.created` 暂时没人订阅）。

> **R4.1 的修改（2026-11-23，GAPS G23）**：事件也开 `mandatory`。退回的事件（没有任何订阅队列）仍然标成已发送（不堵 outbox：
> 系统外的订阅者、`外部: 是` 的事件可能永远没有队列），但不再静默：`last_error = 'unroutable: …'`、
> `factory_outbox_unroutable_total{kind="event",type}`、ERROR 日志。防止丢事件的手段是**系统级的拓扑清单**
> （`factory system` 生成 `contracts/topology.json`，平台在部署服务之前 `factory-runtime topology apply`）：
> 跨项目检查保证系统里每个事件都有订阅方（或者写明 `外部: 是`），清单声明了它们的队列，所以生产环境里退回的事件 =
> 清单没有声明 = 告警。见 boundaries.md §8.1。

### 3.4 顺序：不承诺 ⚖️ E4

如实写出来：**平台不保证任何两条消息的先后**，包括同一个 subject 的。原因：
- 事务的 seq 在提交前分配，提交顺序 ≠ seq 顺序；
- 重试（§4.3）会把失败的消息延后，后面的消息先被处理；
- 一个队列可以有多个消费者（副本 × prefetch）。

单个中继按 seq 发布只是"通常有序"，不是保证。处理器必须写成乱序安全：storefront 的 saga 本来就是一问一答（同一个订单同一时刻只有一条命令在路上），乱序只会出现在重发 / 迟到的情况，由 R3 的条件和 R5 的状态判断兜住。"按 subject 有序"需要单活消费者 + 不能有延迟重试，代价和收益都不划算，不做（记进 out-of-scope）。

---

## 4. RabbitMQ 拓扑和消费

### 4.1 拓扑（平台在启动时声明，幂等）

- **一个 RabbitMQ 每个集群，一个 vhost 每个环境**（`dev` / `staging` / `prod`），和 storefront 设计 §5.2 一致。连接串里带 vhost，服务不感知环境。
- 交换机两个 ⚖️ E5：

| 交换机 | 类型 | 路由键 | 绑定 |
|---|---|---|---|
| `factory.events` | topic | `type`（`order.confirmed`） | 每个订阅者一个队列，按类型绑定 |
| `factory.commands` | direct | `type`（`inventory.reserve`） | 只有拥有这个命令契约的服务绑定（DRC：一个命令恰好一个处理器） |

- **每个（消费服务 × 处理器）一个队列**：`<服务>.<消息类型>`（例 `notification.order.confirmed`）。classic 持久化队列（单节点，按 storefront 的决定），声明 `x-dead-letter-exchange` 指向死信。
- 每个主队列配套：
  - `…​.retry.1` / `.retry.2` / `.retry.3`：队列级 TTL 1s / 10s / 60s，无消费者，`x-dead-letter-exchange: ""` + `x-dead-letter-routing-key: <主队列>` → 到期回到主队列；
  - `….dlq`：死信队列，无 TTL，人来处理。
- 队列级 TTL（不用每条消息的 TTL）：同一个队列里的消息 TTL 相同，不会出现队头的长 TTL 堵住后面的问题。

### 4.2 处理一条消息（生成的代码 + 运行时）

```
收到 delivery
 ├─ 解码 + 契约校验      失败 → 直接发到 .dlq,ack            (毒消息,§4.4)
 ├─ BEGIN
 │   INSERT INTO factory_processed_messages (handler, id) ON CONFLICT DO NOTHING
 │   └─ 冲突 = 处理过了 → COMMIT, ack, 计 duplicate                (传输层去重)
 │   SAVEPOINT ops
 │   执行操作(R3 的执行器,值可以引用 消息.xxx)
 │   └─ 声明过的业务结果(条件不满足 X) → ROLLBACK TO ops, 执行该结果的分支
 │   写 outbox(后续消息)
 │   COMMIT
 └─ ack                                                            (先提交,再确认)
```

- 去重表和业务写入在**同一个事务**：提交了 = 处理过了且记下了；没提交 = 两者都没发生。
- `SAVEPOINT` 让"业务失败"也能**原子地**记下：库存不足时，已经扣掉的前几个 SKU 回滚，但去重记录和 `inventory.reservation_failed` 一起提交（否则失败结果会在重投时被重复发出，或者部分扣减残留）。
- 处理器的超时：一条消息的事务 ≤ 10s（超时 = 暂时性错误）。prefetch 默认 16，同一进程里并发处理（每条一个事务）。

### 4.3 错误分类和重试

| 情况 | 例 | 处理 |
|---|---|---|
| 业务结果（声明过的） | 库存不足、支付被拒 | **不是错误**：走声明的分支，提交，ack |
| 暂时性错误 | 数据库连不上、序列化失败 / 死锁（R3 已在事务内有限重试过）、处理超时 | 重试：发一份副本到 `.retry.<n>`（header `x-factory-attempt: n`，publisher confirm 之后）再 ack 原消息 |
| 用尽 | 第 5 次仍失败（1s, 10s, 60s, 60s） | 发到 `.dlq`（带最后的错误），ack |
| 毒消息 | 解码失败、缺必填字段、未知的版本、DRC 无法表达的数据 | 不重试，直接 `.dlq` |
| 未分类（panic） | 生成代码 / 业务纯函数的 bug | 当暂时性错误处理（会进 DLQ，并计异常指标） |

⚖️ E6：重试用"消费者把副本发到分级的 TTL 队列"，不用 delayed-message 插件（标准镜像里没有，运维多一个依赖），也不用 `nack(requeue=true)`（无退避，坏消息会在队头热循环）。副本发出之后、ack 原消息之前崩溃 → 两份都在 → 去重表兜住。

DLQ 的处理：运行时附带管理子命令 `factory-runtime dlq {list|requeue|drop} --queue … --limit …`（requeue 保留原 id，所以已经处理过的会被去重跳过）。

### 4.4 去重表的保留期和语义层幂等

- `factory_processed_messages` 保留 14 天（每天分批删）。比重试链（< 3 分钟）和人处理 DLQ 的时间都长。
- 去重只认 **id**。同样内容、**新 id** 的消息（R5 的超时重发、人从别处补发）会被当成新消息。所以：

**DRC 规则：命令的处理器必须可重入** ⚖️ E8。编译时检查处理器的每个操作属于以下之一，否则编译失败：
- 幂等新建（R3 `insert_idempotent`）；
- 带条件的更新，且条件会被**它自己的效果**变成假（例：`status = HELD` → 设成 `COMMITTED`）——第二次执行条件不满足，结果声明为 `跳过`；
- 由前一个幂等新建的 `新建` 标志控制（`仅当: 预留.新建`）：只有第一次真正插入时才执行。

事件的处理器（通知类）同样要求可重入，通常一个"以 event_id 为主键的幂等新建"就够了（M11 / M12 的 `notifications.event_id`）。

---

## 5. 设计语言：消息处理器

### 5.1 写法

```yaml
API: ReserveStock                      # M1
种类: 消息处理
收到: inventory.reserve                # 契约从 contracts/ 读
每项: 消息.items                       # 对列表的每个元素执行下面的操作(列表必须有上限,§5.2)
操作:
  - 预留: { 幂等新建: reservations, 键: [order_id, sku],
            值: { order_id: 消息.order_id, sku: 项.sku, qty: 项.qty, status: HELD, expires_at: "now() + 24 小时" } }
  - 扣减: { 更新: stock, 键: { sku: 项.sku }, 仅当: 预留.新建,
            设置: { available: "available - 项.qty", reserved: "reserved + 项.qty" },
            条件: "available >= 项.qty", 不满足: INSUFFICIENT_STOCK }
结果:
  成功:               { 发出: [ inventory.reserved: { order_id: 消息.order_id, items: 消息.items } ] }
  INSUFFICIENT_STOCK: { 发出: [ inventory.reservation_failed: { order_id: 消息.order_id, reason: "INSUFFICIENT_STOCK" } ] }
  RELEASED:           { 当: "任一项 预留.status = RELEASED",   # 幂等新建命中了墓碑(§5.3):没有扣减;结果按"RELEASED → INSUFFICIENT_STOCK → 成功"的顺序判定
                        发出: [ inventory.reservation_failed: { order_id: 消息.order_id, reason: "RELEASED" } ] }
```

```yaml
API: ChargePayment                     # M10
种类: 消息处理
收到: payment.charge
计算: decideCharge                     # AI 写的纯函数:输入 = 消息 + 注入的模拟配置,输出 = { status, reason }
操作:
  - 支付: { 幂等新建: payments, 键: [order_id], 值: { order_id: 消息.order_id, amount: 消息.amount, status: 计算.status, reason: 计算.reason } }
结果:
  成功:   # 幂等新建返回"新插入的行或已有的行":按存下来的结果回答,重复的扣款请求得到第一次的结果
    - 当: "支付.status = SUCCEEDED"  发出: [ payment.succeeded: { order_id: 消息.order_id, amount: 支付.amount } ]
    - 否则:                          发出: [ payment.failed:    { order_id: 消息.order_id, reason: 支付.reason } ]
```

### 5.2 新增的操作语义：`每项`（对列表重复）

R3 的组合命令只有固定个数的操作；M1–M3（以及 R5 的 W5：订单 + N 个明细）需要"对消息 / 请求里的列表逐项执行"。

| 项 | 规定 |
|---|---|
| 语义 | 对列表的每个元素按顺序执行操作组；`项.xxx` 引用当前元素。全部在同一个事务里 |
| 上限（DRC） | 列表必须有个数上限（契约 / proto 的 `个数`），没有上限 → 编译失败 |
| 失败 | 任何一项的任何操作失败 → 整组回滚到 SAVEPOINT（不会只做了一半） |
| 重复元素 | 列表里同一个键出现两次 → 契约校验拒绝（毒消息 / `INVALID_ARGUMENT`），不在操作层合并 |
| 锁顺序 | 执行前按键排序元素（例：按 sku），并发的多 SKU 预留按同一顺序加行锁，避免死锁 |
| 参考求值器 | 在模型状态上逐项执行，同样的回滚语义 |

### 5.3 M2 / M3 和"已释放"的墓碑

- **M2 `inventory.commit`**：每项 `更新 reservations 条件 status = HELD → COMMITTED`，`仅当` 这一步真的更新了才 `stock.reserved -= qty`；条件不满足时再读一次：已是 COMMITTED → `跳过`（重发，可重入）；是 RELEASED → 业务结果 `ALREADY_RELEASED` → 发到 DLQ 并告警（支付已成功但预留被释放：需要人处理。§10 风险 3 说明为什么它不应发生）。
- **M3 `inventory.release`**：每项先 `幂等新建 reservations(status = RELEASED)`：
  - 新插入了 = 预留从未发生 → 留下一个**墓碑**，之后迟到的 `inventory.reserve` 在幂等新建时命中这一行 → 不扣库存，回答 `reservation_failed(RELEASED)`；
  - 已有 HELD → 条件更新为 RELEASED，`stock.available += qty, reserved -= qty`；
  - 已有 RELEASED → `跳过`。

  墓碑让"补偿之后才到的预留"在 inventory 一侧就被挡住，状态机不需要为迟到消息再做补偿（R5 §4.4）。
- 回答 `inventory.committed` / `inventory.released`（storefront 设计 §5.4 的清单里缺这两个事件，saga 图里有；本稿把它们加进契约，并在 storefront 设计书上提修改）。

---

## 6. 运行时（api-factory-runtime v0.4.0）

| 部件 | 内容 |
|---|---|
| outbox 写入 | R3 事务执行器的扩展：提交前写 outbox、`NOTIFY` |
| 中继 | advisory lock 选主、批量发布 + confirm、退避、保留期清理、`mandatory` 的处理 |
| 拓扑 | 启动时按生成的清单声明交换机 / 队列 / 重试 / DLQ（幂等；参数不一致 → 启动失败，不悄悄改） |
| 消费循环 | 连接与重连、prefetch、解码 + 契约校验、去重事务、SAVEPOINT、ack、重试 / DLQ 的分类 |
| 追踪 | 从 envelope 取 `traceparent` 继续 span；处理中发出的消息带当前 span |
| 管理 | `dlq list / requeue / drop`；去重表和 outbox 的清理 |
| 测试钩子 | 故障注入点（§7.3），只在 `FACTORY_TEST_FAULTS=1` 时生效（R3 D3 的约定） |

### 6.1 指标和 SLO

| 指标 | 含义 | 接到的 SLO（storefront 设计 §11.1） |
|---|---|---|
| `factory_outbox_oldest_pending_age_seconds` | 最老的未发送消息的年龄（中继每轮算一次；非主副本不导出） | **outbox 积压**：age < 30s 的时间占比 ≥ 99%；RabbitMQ 宕机实验里应上升、恢复后排空 |
| `factory_outbox_published_total` / `_publish_errors_total` | 发送 / 失败次数 | — |
| `factory_message_lag_seconds{handler}`（直方图） | 提交时刻 − `occurred_at` | **事件消费延迟**；也是"60 秒内完成订单"的分解 |
| `factory_messages_total{handler, outcome}` | outcome = ok / duplicate / business / retry / dlq / poison | duplicate 比例是重投的健康度 |
| `rabbitmq_queue_messages{queue=~".*\\.dlq"}` | DLQ 的长度（RabbitMQ 自带的 prometheus 插件） | > 0 → 工单级告警（不是呼叫级），runbook 写 `dlq list` |
| `factory_handler_duration_seconds{handler}` | 一次处理的耗时 | 容量测试找拐点时用 |

---

## 7. 验证

### 7.1 参考模型：状态 + 消息日志

R3 的参考求值器维护"模型状态"（每张表的行）。R4 加两样：
- **消息日志**：每个提交的写 / 处理在模型里追加它发出的消息（type、subject、data；id 在比较时按出现顺序归一化）；
- **已处理集合**：`(handler, message_id)`。

处理器在模型里是一个函数 `apply(状态, 已处理, 消息) → (状态', 已处理', 发出的消息)`，独立实现 §4.2 / §5 的语义（去重、SAVEPOINT 回滚、每项、仅当、跳过）。投递在模型里没有"中间件"，由测试驱动器决定投递什么、投递几次、什么顺序。

### 7.2 状态差分测试：消息序列

两层，分开预算：

| 层 | 怎么跑 | 规模 |
|---|---|---|
| **进程内** | 测试驱动器直接调用生成的处理器（运行时的 `Delivery` 接口，ack / nack 记在内存里），真实 Postgres，不经 RabbitMQ | 大量随机序列（每个处理器 ≥ 200 个序列 × 3 种子），覆盖语义 |
| **真实 RabbitMQ** | 中继 + 拓扑 + 消费循环 + 重试 / DLQ，服务作为子进程运行 | 少量（每个处理器十几个场景），覆盖投递和崩溃 |

随机序列的生成（在 R3 的"随机请求序列"之上）：
- 消息内容：键取已有的 / 不存在的；数量取边界（正好等于库存、多 1）；列表长度 1 / 多 / 正好上限；
- **重复**：同一条消息（同 id）投 2–3 次，相隔若干条；
- **重发**：同内容、新 id（检验语义层幂等）；
- **乱序**：commit 先于 reserve、release 先于 reserve（墓碑）；
- 与 HTTP 写（W4 调整库存）交错。

每一步之后比较：**全部相关表**、**outbox（新增的消息）**、**去重表**，模型 vs 数据库。覆盖条件：每个处理器的每种 outcome（ok / duplicate / 每个业务结果 / 跳过）都出现；`每项` 里"第 k 项失败"对每个 k 都出现。

### 7.3 故障注入：崩溃点

在 R3 的 `X-Factory-Fail-After: k` 之外，加**进程崩溃点**（钩子调用 `os.Exit(137)`，模拟 kill -9；消息侧通过 AMQP header `x-factory-crash: <点>` 指定，中继侧用环境变量指定第几批）：

| 点 | 期待 |
|---|---|
| 消费：提交之前 | 没有任何写入、没有 outbox；重启后重投、处理一次 |
| 消费：提交之后、ack 之前 | 重启后重投 → duplicate → 状态不变、没有新消息 |
| 消费：发到重试队列之后、ack 之前 | 两份都在 → 只生效一次 |
| 中继：发布之后、confirm 之前 / confirm 之后、标记之前 | 同 id 再发一次 → 下游只生效一次 |
| HTTP 写：outbox 写入之后、提交之前 | 没有写入、没有消息被发出 |
| RabbitMQ 停 30 秒（`docker pause`） | 积压上升；恢复后全部发出；无丢失 |

### 7.4 不变量

| # | 不变量 | 怎么查 |
|---|---|---|
| I1 | 每个**提交了的**写 / 处理发出的消息，在 RabbitMQ 可用后有界时间内 `published_at` 非空 | 测试结束时等待 outbox 排空（上限 10s） |
| I2 | **没提交的**事务没有任何消息出现在 RabbitMQ | 崩溃点场景里，测试端订阅一个绑定 `#` 的旁听队列，按 id 对账 |
| I3 | 效果恰好一次：最终状态 = 模型对**每个不同的 id 恰好应用一次**的结果 | 状态差分 |
| I4 | 语义幂等：同内容的消息应用 1 次和 N 次（新 id）的最终状态相同 | 差分 + 专门的重发用例 |
| I5 | 同一个 id 的多次发送，内容逐字节相同 | 旁听队列 |
| I6 | DLQ 里只有用例故意注入的毒消息 / 用尽的消息 | 测试结束时查 DLQ |
| I7 | 领域不变量：`available ≥ 0`；每个 SKU 的 `available + reserved + 已确认的数量 = 初始 + 调整之和` | 并发场景结束时 |

并发：N（=32）条 `inventory.reserve` 同时争抢同一个热点 SKU（秒杀形状）+ 同一订单的 reserve / release 并发，检查 I3、I7 和"成功次数 ≤ 初始库存"。

### 7.5 变异余量（`make margin` 扩展）

每个变异必须被 ≥3 个用例抓到（崩溃类的变异由 §7.3 的场景计数）：

| 变异 | 应该抓到它的 |
|---|---|
| 去掉去重的 INSERT | 重复投递的差分 |
| 先 ack 再提交 | 崩溃点"提交之前" |
| outbox 在提交之后另开事务写 | 崩溃点"outbox 之后、提交之前"（I2） |
| 中继在 confirm 之前标记已发送 | 中继崩溃 + RabbitMQ 停顿（I1） |
| 缺 SAVEPOINT（业务失败时部分扣减残留） | `每项` 第 k 项失败的差分 |
| 业务失败分支发了成功的消息 | 消息日志的差分 |
| `仅当: 预留.新建` 去掉 | 重发（新 id）的差分（I4） |
| 墓碑改成"不存在就跳过" | release 先于 reserve 的乱序用例 |
| 重试不增加 attempt / 毒消息走重试 | 重试 / DLQ 场景（I6） |
| data 的字段取错列、subject 取错字段 | 消息日志的差分 |
| `每项` 不排序 | 并发场景（死锁 → 超时）|

### 7.6 在哪里跑、花多少资源

- **镜像**：本机已有 `rabbitmq:3.13.7-management`（271MB）和 `postgres:16-alpine`，**不拉新镜像**。本机磁盘只剩约 6.6GB：测试不写持久卷（RabbitMQ 数据目录用 tmpfs，`--tmpfs /var/lib/rabbitmq`）、测试结束删容器。
- **本地**：`make mq-up` / `make mq-down`（AMQP 端口 55672，和 `make db-up` 的 55432 对齐）；单个容器约 150MB 内存。`make doctor` 加一项"RabbitMQ 能连上"。
- **并行**：按工厂既有的并行测试做法隔离共享资源：每个并行 worker 一个 vhost（`rabbitmqctl add_vhost t<worker>`，结束删除）+ 一个数据库，worker 数 ≤ 6（12 核里留一半给 Postgres / RabbitMQ）。改并行后确认结果与串行一致。
- **时间预算**：进程内差分 ≤ 2 分钟；真实 RabbitMQ 的场景 ≤ 3 分钟（崩溃场景每个要重启子进程，约 1s）；margin 只对消息相关的变异跑真实 RabbitMQ 场景。
- **CI**：verify 在 self-hosted runner（laptop）上跑，RabbitMQ 作为服务容器（同一个镜像 tag）；`make projects` 的 storefront 样例验收也接上。

---

## 8. 和 R3 的接口（不改 R3 的语义）

- R3 的执行器多两个输入：`发出` 的消息列表、可选的 `消息`（处理器的上下文）。
- R3 的错误映射不变；消息处理器里错误码不出现在响应里，而是映射成 §4.3 的处理（`FAILED_PRECONDITION` + 声明的原因 → 业务结果；`ABORTED` / 连接错误 → 暂时性；`INVALID_ARGUMENT` → 毒消息）。
- R3 的状态差分测试框架直接扩展（模型状态多了消息日志和已处理集合）。

---

## 9. 实施顺序

| 步 | 内容 | storefront | 中立样例（工厂的 `design/`） |
|---|---|---|---|
| R4-1 | 消息契约（`消息.yaml` → JSON Schema + 锁文件）、写操作的 `发出`、outbox + 中继 + 拓扑、指标；故障注入（outbox 之后 / 中继崩溃）；`make mq-up` | — （W5 的事件在这里能生成，W5 整体在 R5 计入） | 新建任务 → `task.created` |
| R4-2 | 消息处理器：消费循环、去重、ack after commit、重试 / DLQ、毒消息、可重入 DRC；进程内消息序列差分 + 真实 RabbitMQ 场景 | M11、M12（幂等新建）、M10（幂等新建 + 纯函数 + 按结果分支） | 订阅 `task.completed` → 写一条活动记录 |
| R4-3 | `每项`、SAVEPOINT 业务分支、`仅当`、墓碑；并发（热点 SKU）、乱序、重发；变异余量 | M1、M2、M3 | 余额足够才扣（`account.debit` 命令，多个账户逐项、全部或全不） |

完成后 storefront 19 / 28（68%）。样例项目新增 `examples/storefront-v2/contracts/`。

---

## 10. 风险

| # | 风险 | 应对 |
|---|---|---|
| 1 | RabbitMQ 让测试变慢、变不稳定（端口、启动时间、残留队列） | 大部分语义在进程内差分里验证；真实 RabbitMQ 只做投递 / 崩溃；每个 worker 独立 vhost，用完删除 |
| 2 | "不承诺顺序"让处理器的作者（包括 AI）写出依赖顺序的逻辑 | 差分测试本来就投递乱序的序列；可重入 DRC 在编译时拦住"非幂等的操作" |
| 3 | M2 遇到"预留已释放"（支付成功但库存被释放） | 只有当预留的过期时间短于 saga 的最长停留时才会发生：过期时间设为 24 小时（只当安全网），saga 的期限见 R5；仍发生 → DLQ + 告警，由人处理（退款流程是"外"） |
| 4 | 命令契约归接收方，发送方（order）要等 inventory 先发布契约 | 系统级的共享 `contracts/` + 锁文件；样例项目里 inventory 先做（R4-3）、order 在 R5 |
| 5 | 平台表（`factory_*`）和项目的表名冲突 / 迁移顺序 | 固定前缀 + DRC 禁止项目表用这个前缀；生成的迁移文件名排在最后 |

---

## ⚖️ 需要决定

| # | 问题 | 推荐 |
|---|---|---|
| E1 | 平台表（outbox、去重）怎么建 | 工厂生成到项目的 `db/schema/zz_factory_platform.sql`，走项目的迁移流程（看得见、可评审；不在运行时启动时偷偷建表） |
| E2 | 消息 id 的格式 | UUIDv7（数据库 `uuid` 类型，按时间有序便于排查）；storefront 设计里的 `evt_01J…` 示例改成 UUID 文本 |
| E3 | 中继的并发 | 每个数据库一个活动中继（advisory lock 选主），不用 `SKIP LOCKED` 多中继：吞吐足够，且不在持有行锁时做网络 IO |
| E4 | 顺序保证 | 不承诺任何顺序；处理器必须乱序安全（由状态判断）。按 subject 有序记进 out-of-scope |
| E5 | 交换机 | 两个：`factory.events`（topic，可有 0..N 个订阅者）和 `factory.commands`（direct，`mandatory`，恰好一个处理器） |
| E6 | 重试的实现 | 消费者把副本发到分级 TTL 队列（1s / 10s / 60s），最多 5 次 → DLQ；不用 delayed-message 插件、不用 requeue |
| E7 | 契约文件放在哪 | 系统级共享目录 `contracts/`（生产者写、消费者读，锁文件检查兼容性），项目用 `factory.yaml` 的 `contracts:` 指向 |
| E8 | 重发（同内容、新 id）的语义 | 新 id + "命令的处理器必须可重入"的 DRC；不让重发复用旧 id（复用会被去重跳过，卡住的 saga 仍然卡住） |

---

## 实现记录（2026-11-22）

和上面的设计稿不同的地方（都是缩小或更具体，不改语义）：

| 设计稿 | 实现 | 理由 |
|---|---|---|
| 处理器的入口不是 RPC | proto 里另外一个服务 `<领域>MessageHandlerService` 列出处理器（`rpc X(XRequest) returns (XResponse)`，请求 = 契约的 data），**不挂载**；生成的处理器是 `consume.Handler` | 复用工厂的 proto → IR → 校验（protovalidate）的整条路径；buf lint 的命名规则 |
| 契约的 data 字段由消费方从 JSON Schema 读 | JSON Schema 里带 `x-factory`（字段的声明原样），消费方读它生成自己的 `XRequest` | 不写 JSON Schema 的反向解析器;Schema 本身照样给别的语言的消费方用 |
| `版本: [1, 2]`（并行期两个版本都发） | 一个类型同时只有一个版本 | 样例里没有需要;见 GAPS |
| 契约里的枚举 | 不支持（用 文本） | 枚举属于领域，跨服务共享要另外的规则;见 GAPS |
| M10 的 `计算: decideCharge`（AI 的纯函数） | `如果 消息.amount > 100000 则 FAILED 否则 SUCCEEDED`（设计语言的表达式） | 模拟支付的规则一句话就能写，不需要 AI |
| `expires_at: "now() + 24 小时"` | 表的默认值 `DEFAULT (now() + interval '24 hours')` | 表达式里还没有时间的加减（见 GAPS） |
| 结果的 `当` 和 `每项` 里的失败 | 某一项 `不满足` → 立刻回滚，结果 = 那个原因码;`当` 只在全部做完之后判定 | 先到的失败决定结果，判定顺序固定 |
| 进程内的测试驱动 | 运行时的 `POST /factory.test/deliver`（只在 `FACTORY_TEST_FAULTS=1` 时响应），驱动按 HTTP 投递 | 生成的服务是子进程，驱动不链接生成代码 |
| 真实 RabbitMQ 的场景（每个处理器十几个） | 每个 API 4 个（投递 → 处理 → 发出;同一个 id;毒消息 + requeue;重试之后成功），`make accept-mq`（独立 vhost，串行） | 语义在进程内的差分里验证;投递和崩溃点由运行时的集成测试覆盖（`consume_int_test.go` 的崩溃点、重试分级、DLQ 管理;`outbox` 的中继崩溃点） |
| 重试成功的路径 | 测试 header `x-factory-fail-attempts: n`（前 n 次失败） | `x-factory-fail-after` 会被重试的副本带着，每次都失败 |
