# API Factory — 查询工厂

式样书驱动的 API 工厂流水线。人只写式样书和 review,其余全部自动化。

## 范围

一个工厂对应所有问题不现实也没必要。工厂成立的条件是 **数量多 × 形状稳定 × 判定能机械化**,
所以按后端工作的类型分成多条生产线,共用一个工厂平台(领域层 / 审查 / 闸门 / 试验)。

| 生产线 | 状态 | 对象 |
|---|---|---|
| **查询工厂**(本仓库) | 开发中 | 只读、同步请求/响应、单表 + 多对一的关联 + 一对多的嵌套 / 子聚合(以后: 分组聚合) |
| 命令工厂 | 计划 | 写入: 校验 → 业务规则 → 写入操作(操作作为数据返回,平台执行) |
| IF 工厂 | 候选 | 文件布局 ↔ 表(编码、定长、补零),golden file / 往返判定 |

**查询工厂不适用**: 写入、多表 JOIN、分组聚合(`分组` / `排名`: 件数 / 合计 / 最小 / 最大 / RANK)以外的复杂查询(→ `datasource.sql` 逃生口)、
外部 API 调用、流式、算法类逻辑(→ `custom`,AI 辅助人开发)。
逃生口的使用比例(**逃生率**)由 `factory stats` 显示: 逃生率高说明范围选错了或者缺模式。

```
① 式样书闸门 → ② 确定性生成 → ③ AI 实现 → ④ 路径闸门 → ⑤ 编译 + 验收 → ⑥ 部署(镜像 → 新版本 → 检查 → 切换)
```

整条流水线(GitHub + Cloud Run)见下面的「流水线(CI/CD)与部署」。本地模拟: 式样书合并 → 新版本在服务约 90 秒。

## 快速开始

```sh
brew install go bufbuild/buf/buf sqlc   # 另需 docker 和 claude CLI
make db-up                              # 本地 Postgres(端口 55432)
make loop SPEC=specs/jobs/get-job-status/spec.yaml          # MODEL=claude-sonnet 可让实现用便宜一些的模型(审查总是强模型)
```

## 流水线(CI/CD)与部署

```
式样书的 PR ──→ 合并 ──→ 生成物的 PR(机器人)──→ 合并 ──→ 部署
spec-review     generate                verify               deploy
检查 + AI 审查   生成代码 + PR 说明        生成物是最新的        镜像 → 新版本(不切流量)
                (AI 只写计算 / 自定义)     + 验收(3 种子)+ margin  → 健康检查(版本号)/ 认证 → 切换
```

| 阶段 | 脚本 | GitHub Actions | 说明 |
|---|---|---|---|
| 式样书的 PR | `scripts/ci/spec-review.sh` | `spec-pr.yml` | 机械检查 + 确定性生成 + AI 审查(结果贴到 PR) |
| 合并后: 生成 | `scripts/ci/generate.sh` | `generate.yml` | 生成物的分支 `factory/gen-<sha>` + PR 说明(管理者看这一页,不用逐行读生成的代码) |
| 生成物的 PR | `scripts/ci/verify.sh` | `verify.yml` | 生成物和式样书一致 + 编译 + 单元测试 + 验收 3 种子 + 变异余量 |
| 合并后: 发布(GitOps) | 共用的 workflow | `release.yml` | 验证 → API 和建表的镜像(只构建一次,tag = 提交,Trivy / SBOM)→ gitops-manifests 的 dev 更新 → Argo CD 同步 |
| (另一种)Cloud Run | `scripts/ci/deploy.sh` | `deploy.yml` | `TARGET=cloudrun`: Cloud Build → `--no-traffic` 的新版本 → 只读检查 → 切流量(GCP 的设定有了才运行) |

- GitOps(MIERAI/gitops-platform-blueprint): 镜像 / 推送是共用的 workflow;集群的状态在 MIERAI/gitops-manifests
  (`api-factory/overlays/{dev,prod}`: Postgres → 建表 Job(`Dockerfile.schema`)→ API → 部署后的检查);prod = promote 的 PR + 人批准;
  本地的集群: MIERAI/infrastructure-live(kind + Argo CD)
- workflow 只装工具、拿凭证、开 PR,逻辑都在脚本里: **本地跑的和 CI 跑的是同一套**
- 本地模拟整条: `scripts/ci/sim.sh`(本地的 bare 仓库当 GitHub,docker 模拟 Cloud Run;部署后对新版本跑全部验收,`SIM_BREAK=1` 确认回滚)
- 镜像: `Dockerfile`(distroless,约 20MB)。`/healthz` 的响应头 `X-App-Version` = 版本
- 认证: `AUTH_MODE` 必须指定(不写不启动)。`token` = Bearer 令牌(服务器只存 SHA-256,`AUTH_TOKEN_SHA256` 逗号隔开可轮换)/
  `none` = 交给平台(Cloud Run 的 IAM `--no-allow-unauthenticated`)
- 在 GitHub 上启用(仓库的 variables / secrets;没设时对应的 workflow 跳过):
  - `FACTORY_GENERATE=on`: 式样书的审查(spec-pr)和生成(generate)在 self-hosted runner `[self-hosted, laptop]` 上跑
    (tailnet 里的常驻机器;仓库是私有的,外部的 PR 不会在上面执行)。模型 = factory.yaml 的 Claude(审查 claude 阻塞,实现 claude-sonnet;
    实现由路径闸门 + 编译 + 验收兜底)。runner 上预装 `claude` CLI 和 docker,密钥 CLAUDE_CODE_OAUTH_TOKEN。
    生成物的 PR 用 `GITHUB_TOKEN` 开(不会触发 verify)→ generate 在同一次运行里对生成物的分支调用 verify,结果贴到 PR
  - `GCP_PROJECT` / `GCP_REGION` / `SERVICE` / `AR_REPO` / `RUN_SA` / `GCP_WIF_PROVIDER` / `GCP_DEPLOY_SA`(Workload Identity Federation,仓库里不放密钥);
    连接信息在 Secret Manager(`<SERVICE>-database-url` 等)。第一次部署(服务还不存在)不能用 `--no-traffic`,先普通部署一次
- 真实环境的部署后检查只做只读的(健康检查 / 认证);对新版本跑验收会清空并灌测试数据,只用于模拟

## 其他项目(给另一个仓库 / 另一个数据库生成服务)

工厂不只给本仓库生成。一个**项目** = 一个 Go 模块 + 它自己的数据库,目录结构和本仓库一样(`design/`、`db/schema/` 由人写,
其余由工厂生成)。生成的代码只依赖项目自己的模块和公开的运行时 [api-factory-runtime](https://github.com/MIERAI/api-factory-runtime),
工厂本身(生成器、核心代数、审查、验证)不进入项目。

```sh
factory init ../shop/services/catalog --module github.com/you/shop/services/catalog   # 脚手架: go.mod、buf、cmd/server、factory.yaml
# 写 design/<领域>/ 和 db/schema/*.sql
factory --project ../shop/services/catalog design
factory --project ../shop/services/catalog check
factory --project ../shop/services/catalog prepare        # → proto、gen/、internal/handler、cmd/server/wiring_gen.go
```

- `--project DIR`: 工厂切换到项目根目录;工厂自己的资源(factory.yaml 的默认值、适配器)仍从工厂目录读(`factory/proj`)
- 项目的 `factory.yaml` 只写项目的键(proto 的包名前缀),按键覆盖工厂的;生成代码的模块名 = 项目 go.mod 的 module
- `examples/` 下的项目(现在: storefront v2 的样例 catalog / inventory / order / payment / notification,见 `docs/design/storefront-v2-readiness.md`)由 `make projects` /
  CI 的 verify ⑤ 检查: 设计 → 生成物是最新的 → 编译 → 用项目自己的数据库验收。工厂遇到做不到的,记在 `examples/storefront-v2/GAPS.md`

## 架构: 各环节通过文件契约串起来

```
式样书 + 领域层 + proto + schema
        │
   [编译器] factory/compiler        语言无关。SQL 分析只用 sqlc 的 json 输出
        │
   API 计划(IR)  .factory/plan.json  ← 契约(factory/ir,有版本号)
        │      + db/queries/*.sql
   ┌────┼──────────────┬───────────────────┐
[验收生成]          [审查]           [语言适配器] factory/adapters/golang
factory/acceptgen   factory/prompts   IR → Go 代码(gen/db 查询函数 / buf / handler / enum / wiring)
语言无关             review.md         清单 manifest.yaml: AI 改哪些文件 / 闸门规则 / prompt / 工具
   │                                          │
gen/acceptance/*.yaml                 [AI 实现] factory ai(模型和角色在 factory.yaml)
   │                                          │
   │                                  [闸门] factory guard(规则来自清单)
   │                                          │
   └──────────→ [验收执行器] factory accept ←─┘   黑盒: 只要 URL + fixture 用的 DB + IR
                 [测试环境]   factory serve-test   启动被测服务器(DB 会话关掉索引扫描)
```

| 环节 | 包 | 输入 → 输出 | 换什么时要改 |
|---|---|---|---|
| 编译器 | `factory/compiler` | 式样书 / 领域层 / proto / schema → IR + SQL | 式样书格式 |
| 验收生成 | `factory/acceptgen` | IR → `gen/acceptance` | 模式的测试策略 |
| 审查 | `factory/prompts/review.md` | 式样书 + IR 派生物 → findings | 审查规则 |
| 语言适配器 | `factory/adapters/<lang>` + `manifest.yaml` | IR → 代码 | 换语言: 写新适配器,改 `factory.yaml` 的 adapter |
| AI 调用 | `factory/ai` + `factory.yaml` | prompt → 结果 | 换模型: 只改 `factory.yaml` |
| 闸门 | `factory/guard` | git 改动 + 清单 → 违规 | 规则在清单里 |
| 验收执行器 | `factory/accept` | URL + DB + IR → 结果 | 不用改(任何说 Connect 的服务都能测) |
| 运行时平台 | `platform/` | — | 换语言时由适配器提供对应的平台 |

| 层 | 路径 | 谁来写 |
|---|---|---|
| 式样书 / 领域层 | `specs/<domain>/`, `proto/`, `db/schema/<domain>.sql`(当前结构,工厂读)+ `db/migrations/`(goose 迁移,线上执行;CI 检查两者无漂移) | 人 |
| 确定性生成 | `gen/`, `db/queries/`, `*_gen.go`, `.factory/plan.json` | 工厂(勿手改) |
| 业务实现 | 清单 `ai_files` 指定的文件(compute 的逻辑函数等) | AI |

## 模式(一个 API 由哪些部件组成)

```
请求 → [校验] → [输入映射] → [取数] → [模式逻辑 / 业务逻辑] → [输出映射] → 响应
       proto      名字约定     sqlc     模式插件  / AI 纯函数     名字约定
```

| pattern | handler 由谁写 | AI 写什么 | 例 |
|---|---|---|---|
| `get_by_id` | 工厂 | 无 | GetJobStatus |
| `list_keyset` | 工厂(游标/ORDER BY/LIMIT 追加到查询,token 签名并绑定筛选条件) | 无 | ListJobs |
| `list_all` | 工厂(多条: 筛选 + 排序 + 上限,超过上限报错) | 无 | ListStoreSales |
| `top_n` | 工厂(前N条) | 无 | ListTopStores |
| (多条 / 前N条 / 一览 + `分组` / `排名`) | 工厂(GROUP BY + 件数 / 合计 / 最小 / 最大,RANK()) | 无 | ListTopLineStores、ListStoreLineRanking |
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
| SQL → 查询函数 | 工厂(核心代数的 SQL 后端;逃生口 SQL 用 sqlc 的分析取类型) |
| 参数校验 | protovalidate(规则写在 proto 里) |
| 错误转换、超时、日志 | `platform/` 拦截器 |
| handler 骨架、wiring | `factory prepare` |
| 请求↔查询↔响应的映射 | `factory prepare`(proto 描述符 + gen/db 的 AST) |
| 分页(游标、多取一行、token 签名/绑定) | `list_keyset` 插件 + `api-factory-runtime/paging` |

## 模型对照试验

```sh
scripts/trial.sh claude specs/jobs/get-job-summary/spec.yaml 3   # 开发用(快)
scripts/trial.sh local  specs/jobs/get-job-summary/spec.yaml 3   # 最终验证: 本地模型(factory.yaml 的 ai.models.local)
```
结果追加到 `.factory/trials/results.jsonl`,每次的 prompt/diff/闸门/验收输出在 `.factory/trials/<run>/`。
