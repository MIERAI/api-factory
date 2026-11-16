# API Factory

式样书驱动的 API 工厂流水线。人只写式样书和 review,其余全部自动化。

```
① 式样书闸门 → ② 确定性生成 → ③ AI 实现 → ④ 路径闸门 → ⑤ 编译 + 验收 → (⑥ 部署: 未接)
```

## 快速开始

```sh
brew install go bufbuild/buf/buf sqlc   # 另需 docker 和 claude CLI
make db-up                              # 本地 Postgres(端口 55432)
make loop SPEC=specs/jobs/get-job-status/spec.yaml
```

## 目录与分层

| 层 | 路径 | 谁来写 | 换公司时 |
|---|---|---|---|
| 式样书 | `specs/**/spec.yaml`, `proto/` | 人(review 后合并) | 每家公司各自写 |
| 工厂核心 | `factory/spec` `factory/guard` `factory/accept` `factory/prompts` | 平台 | **不变**(资产) |
| 模板适配器 | `factory/prepare` | 平台 | 换语言时替换 |
| 运行时模板 | `platform/` | 平台 | 换语言时替换 |
| DB 结构 | `db/schema.sql` | 人 | 每家公司各自维护 |
| 确定性生成 | `gen/` `db/queries/` `*_gen.go` | 工厂(勿手改) | — |
| 业务实现 | `internal/handler/**`(只限 `ai_may_edit`) | AI | — |

## 闸门

- **式样书闸门** `factory check` + `buf lint`: 必填项、rpc 是否存在于 proto、read_only 不得写入、fixture 表必须已声明、ai_may_edit 必须在 `internal/handler/` 下
- **路径闸门** `factory guard`: 改动必须在 `ai_may_edit` 内;禁止 import `database/sql`/`pgx`/`net/http`/`os/exec`;禁止 SQL 字面量
- **验收** `factory accept`: 按式样书灌 fixture、通过 Connect HTTP/JSON 调用、比对 code 和响应。与运行时语言无关

## 不交给 AI 的东西(确定性处理)

| 内容 | 由谁处理 |
|---|---|
| 类型、路由、序列化 | buf + protoc-gen-go / connect-go |
| SQL → 查询函数 | sqlc(SQL 来自式样书) |
| 参数校验 | protovalidate(规则写在 proto 里) |
| 错误转换、超时、日志 | `platform/` 拦截器 |
| handler 骨架、wiring | `factory prepare` |
