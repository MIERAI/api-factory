# storefront v2 样例项目里发现的工厂缺口

做样例项目时遇到的"工厂做不到 / 做得不好"的地方。每条记：发现时间、形状、当时怎么绕过、要不要修。
(readiness 文档 `docs/design/storefront-v2-readiness.md` 的"探索工具的不足")

| # | 发现 | 形状 | 绕过 | 处理 |
|---|---|---|---|---|
| G1 | R1 catalog | 枚举列是 `SMALLINT`(int16)时生成失败:`query column row.Status (int32) cannot be mapped to enum field status (DB type int16)`。枚举只支持 int32 的列 | 表结构改用 `INTEGER` | 待修:状态类的小整数列很常见(SMALLINT 省空间),类型注册表里整数16 应该能当枚举的数据库类型 |
