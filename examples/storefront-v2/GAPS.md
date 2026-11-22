# storefront v2 样例项目里发现的工厂缺口

做样例项目时遇到的"工厂做不到 / 做得不好"的地方。每条记：发现时间、形状、当时怎么绕过、要不要修。
(readiness 文档 `docs/design/storefront-v2-readiness.md` 的"探索工具的不足")

| # | 发现 | 形状 | 绕过 | 处理 |
|---|---|---|---|---|
| G1 | R1 catalog | 枚举列是 `SMALLINT`(int16)时生成失败:`query column row.Status (int32) cannot be mapped to enum field status (DB type int16)`。枚举只支持 int32 的列 | 表结构改用 `INTEGER` | 待修:状态类的小整数列很常见(SMALLINT 省空间),类型注册表里整数16 应该能当枚举的数据库类型 |
| G2 | R2-2 order(Q6) | 订单历史的"明细金额合计" = Σ(qty × price)。子聚合的 `合计(列)` 只能对一列求和,不能写表达式 | `order_items.amount` 用数据库的生成列(`GENERATED ALWAYS AS (qty * price) STORED`),合计(amount)。差分测试的造数据不插入生成列 | 暂不修:生成列让"金额 = 数量 × 单价"由数据库保证,比在查询里算更稳。真有第二个需求再考虑聚合里的表达式 |
| G3 | R2-2 order | 嵌套的子行 / 子聚合的列不能是枚举(枚举的映射按来源的表查) | Q5 的明细没有枚举列,没有绕 | 待修:子表的枚举列(例: 明细的状态)出现时再做(子行的消息里映射枚举) |
