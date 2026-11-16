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

## 模式(一个 API 由哪些部件组成)

```
请求 → [校验] → [输入映射] → [取数] → [模式逻辑 / 业务逻辑] → [输出映射] → 响应
       proto      名字约定     sqlc     模式插件  / AI 纯函数     名字约定
```

| pattern | handler 由谁写 | AI 写什么 | 例 |
|---|---|---|---|
| `get_by_id` | 工厂 | 无 | GetJobStatus |
| `list_keyset` | 工厂(游标/ORDER BY/LIMIT 追加到查询,token 签名并绑定筛选条件) | 无 | ListJobs |
| `compute` | 工厂(映射 + 查询 + 调用逻辑) | 纯函数 `<method>Logic(req, rows)` | GetJobSummary |
| `custom` | AI(逃生口) | 整个 handler | — |

映射默认按名字自动对应(`job_type` ↔ `JobType`),对不上时在 `pattern.mapping` 里写。

## 闸门

- **式样书闸门** `factory check` + `buf lint`: 必填项、rpc 是否存在于 proto、read_only 不得写入、fixture 表必须已声明、ai_may_edit 必须在 `internal/handler/` 下
- **路径闸门** `factory guard`: 改动必须在 `ai_may_edit` 内;禁止 import `database/sql`/`pgx`/`net/http`/`os/exec`;禁止 SQL 字面量
- **纯函数闸门**(compute 的逻辑文件): 禁止 `context`/`os`/`net`/`sync`/`math/rand`/平台包;禁止 `time.Now` 等和 goroutine
- **验收** `factory accept`: 按式样书灌 fixture、通过 Connect HTTP/JSON 调用、比对 code 和响应。与运行时语言无关

## 不交给 AI 的东西(确定性处理)

| 内容 | 由谁处理 |
|---|---|
| 类型、路由、序列化 | buf + protoc-gen-go / connect-go |
| SQL → 查询函数 | sqlc(SQL 来自式样书) |
| 参数校验 | protovalidate(规则写在 proto 里) |
| 错误转换、超时、日志 | `platform/` 拦截器 |
| handler 骨架、wiring | `factory prepare` |
| 请求↔查询↔响应的映射 | `factory prepare`(proto 描述符 + sqlc 生成代码的 AST) |
| 分页(游标、多取一行、token 签名/绑定) | `list_keyset` 插件 + `platform/paging` |

## 模型对照试验

```sh
scripts/trial.sh claude specs/jobs/get-job-summary/spec.yaml 3   # 开发用(快)
scripts/trial.sh local  specs/jobs/get-job-summary/spec.yaml 3   # 最终验证: 本地模型(winpc Ollama)
```
结果追加到 `.factory/trials/results.jsonl`,每次的 prompt/diff/闸门/验收输出在 `.factory/trials/<run>/`。
