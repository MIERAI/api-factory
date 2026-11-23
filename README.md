# API Factory — 查询工厂

式样书驱动的 API 工厂流水线。人只写式样书和 review,其余全部自动化。

## 范围

一个工厂对应所有问题不现实也没必要。工厂成立的条件是 **数量多 × 形状稳定 × 判定能机械化**,
所以按后端工作的类型分成多条生产线,共用一个工厂平台(领域层 / 审查 / 闸门 / 试验)。

| 生产线 | 状态 | 对象 |
|---|---|---|
| **查询工厂**(本仓库) | 开发中 | 只读、同步请求/响应、单表 + 多对一的关联 + 一对多的嵌套 / 子聚合 + 分组聚合 |
| **命令工厂**(本仓库) | R3 完成 | 写入: 校验 → 写操作作为数据 → 运行时在一个事务里执行 → 失败的原因 → 错误码。R3-1: 新建 / 按键更新 / 按键删除 + 乐观锁;R3-2: 组合命令(多个写操作一个事务,引用前面的返回值)、条件更新(业务原因码)、幂等新建(设计 `docs/design/r3-command-factory.md`)。验证: 状态差分 + 故障注入 + 并发不变量 + 重放 |
| IF 工厂 | 候选 | 文件布局 ↔ 表(编码、定长、补零),golden file / 往返判定 |

**查询工厂不适用**: 写入(→ 命令工厂)、多表 JOIN、分组聚合(`分组` / `排名`: 件数 / 合计 / 最小 / 最大 / RANK)以外的复杂查询(→ `datasource.sql` 逃生口)、
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
| 生成物的 PR | `scripts/ci/verify.sh` + `scripts/ci/margin.sh` | `verify.yml` | 生成物和式样书一致 + 编译 + 单元测试 + 验收 3 种子 + 变异余量(按变异分 4 个并行 job;PR 1 个种子、合并后和每月 3 个种子,都带结果缓存 + 早停) |
| 合并后: 发布(GitOps) | 共用的 workflow | `release.yml` | 验证 → API 和建表的镜像(只构建一次,tag = 提交,Trivy / SBOM)→ gitops-manifests 的 dev 更新 → Argo CD 同步 |
| 每月 | 同上 | `margin-monthly.yml` | main 的全量验证(每月 2 日;也可手动触发)(3 个种子);失败 → 开 issue(标签 `margin-monthly`,列出不够 3 次的变异) |
| (另一种)Cloud Run | `scripts/ci/deploy.sh` | `deploy.yml` | `TARGET=cloudrun`: Cloud Build → `--no-traffic` 的新版本 → 只读检查 → 切流量(GCP 的设定有了才运行) |

### 式样书 PR 的 AI 审查: 审查哪些 API / 推送前自己跑 / 本地回答 CI 的问题

- **审查哪些 API**: `factory review-changed --base origin/main` = 审查的输入和 base 不同的 API。审查的输入 =
  API 的设计书 + 它的审查记录 + 领域里**它引用的部分**(设计书里出现的表: 整个表定义 = 列 / 主键 / 规则 …;这些表的列用到的枚举
  和设计书里直接写的枚举;时区)+ 领域的审查记录(`factory/review/fingerprint.go`)。领域.yaml 里加一张无关的表 / 改无关的表 →
  这些 API 不重审(审查不确定: 已经通过的设计书重审会冒出新的挡流水线的问题)
- **沿用**: 同一个 PR 里,上面的输入 + prompt 模板 + 模型(`scripts/ci/review-key.sh` → `factory review-key`)和上次通过时相同 → 不调用模型
- **项目的设计书也审查**: `examples/` 下有 `factory.yaml` 的目录(`scripts/projects.sh --list`,`factory --project` 的项目)和本仓库一样:
  生成 → `factory --project <项目> review-changed`(项目自己的 `design/` 和 base 比)→ 审查。结果在 `<项目>/.factory/reviews`,
  回答写进 `<项目>/design/<领域>/X.审查.yaml`。spec-pr 的触发路径包括 `examples/**/design/**`
- **推送前自己跑**(和 CI 同一个脚本,本地的 `claude` CLI;会像 `make prepare` 一样在工作区生成文件):
  ```sh
  make review-local                  # = BASE=origin/main REVIEW_MODEL=claude scripts/ci/spec-review.sh;结果 .factory/review-local.md
  go run ./factory/cmd/factory review-changed -v --base origin/main   # 只看会审查哪些 API(不调用模型;项目: --project <项目>)
  ```
- **本地回答 CI 的问题**: spec-pr 把 `.factory/reviews` 和 `<项目>/.factory/reviews` 作为 artifact `spec-reviews` 上传(保留 14 天)。下载到本地后 `factory answer` 照常用:
  ```sh
  export GH_TOKEN=$(gh auth token --user MIERAI)    # gh 能读这个仓库的令牌即可
  make review-fetch PR=25                           # = scripts/review-fetch.sh --pr 25(默认当前分支;--branch B / --run ID)
  go run ./factory/cmd/factory answer --list design/sales/ListX.yaml
  go run ./factory/cmd/factory answer design/sales/ListX.yaml F1=2 F2=@design:改成了一览 F3=keep:"0 件时返回空列表"
  go run ./factory/cmd/factory answer examples/storefront-v2/catalog/design/catalog/CreateProduct.yaml F1=1   # 项目: 自动 --project
  git add design/ examples/ && git commit && git push   # 审查记录(X.审查.yaml)和轮次记录(X.审查轮次.jsonl)随 PR 提交
  ```
  回答的写法: `F1=2`(选项)/ `F1=keep:"现在的行为"`(保持现在的行为,回答和补充规则写成这段文字,不是"保持现在的行为"这个标签)/
  `F1=2:"实际的结果"`(选项 2,规则的结果写成这段文字)/ `F1="自己写"` / `F1=@design[:改了什么]` / `0` 跳过
- **不再循环提问**(`factory/review`,测试 `factory/review/loop_test.go`,不调用模型):
  - **一份轮次历史**: 轮次 = 审查目录 `<UTC 时间>-<模型>-<API>`(本地的 `.factory/reviews`、CI 按分支缓存的、review-fetch 下载的)
    ∪ 提交的轮次记录 `design/<领域>/X.审查轮次.jsonl`(`review-result` 每轮写入;`review-fetch` 之后 `factory review-sync` 把 CI 的轮次写进去)。
    轮次记录只留"最后一次通过 + 之后的轮次",不进审查的指纹(提交它不会让 API 重审)。本地第 2 轮之后推送,CI 接着算第 3 轮
  - **按编号认回答**: `factory answer` 在记录里写 `位置` 和 `问题编号`(`<轮次 ID>#F1`,包括之前轮次里的同一个问题)。
    一个问题算"已经回答" = 编号在记录里 / 原文相同 / 位置 + 选项相同;CI 换了说法的问题也认得出来
  - **审查员看到全部回答**(编号 A1… = API 的记录,D1… = 领域的记录),被要求不再问回答覆盖的问题、不对回答本身追问;
    它输出的 `same_as: A3` 编号存在才算,这个问题直接丢弃(重复提问)。不靠模型的部分: 原文 / 位置 + 选项相同 → 丢弃
  - **封顶**: 连续没通过超过 `review.max_rounds`(factory.yaml,默认 3)轮以后,新的 major 记为"封顶"(显示、可以回答,不挡);
    blocker 和之前挡了、还没回答的问题照常挡

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
    (tailnet 里的常驻机器;仓库是公开的: fork 来的 PR 不在上面执行,外部贡献者的 PR 要批准才运行)。模型 = factory.yaml 的 Claude(审查 claude 阻塞,实现 claude-sonnet;
    实现由路径闸门 + 编译 + 验收兜底)。runner 上预装 `claude` CLI 和 docker,密钥 CLAUDE_CODE_OAUTH_TOKEN。
    生成物的 PR 用 `GITHUB_TOKEN` 开(不会触发 verify)→ generate 在同一次运行里对生成物的分支调用 verify,结果贴到 PR
  - `GCP_PROJECT` / `GCP_REGION` / `SERVICE` / `AR_REPO` / `RUN_SA` / `GCP_WIF_PROVIDER` / `GCP_DEPLOY_SA`(Workload Identity Federation,仓库里不放密钥);
    连接信息在 Secret Manager(`<SERVICE>-database-url` 等)。第一次部署(服务还不存在)不能用 `--no-traffic`,先普通部署一次
- 真实环境的部署后检查只做只读的(健康检查 / 认证);对新版本跑验收会清空并灌测试数据,只用于模拟

## 其他项目(给另一个仓库 / 另一个数据库生成服务)

工厂不只给本仓库生成。一个**项目** = 一个 Go 模块 + 它自己的数据库,目录结构和本仓库一样(`design/`、`db/schema/` 由人写,
其余由工厂生成)。生成的代码只依赖项目自己的模块和公开的运行时 [`runtime/`](runtime/)(Go 模块
`github.com/MIERAI/api-factory/runtime`,Apache-2.0),工厂本身(生成器、核心代数、审查、验证)不进入项目。

### 一个产品: 生成器 + 运行时 + 验证(v0.6.0 起)

- **运行时在本仓库的 `runtime/`**,是独立的 Go 模块 `github.com/MIERAI/api-factory/runtime`(v0.5.2 之前在
  MIERAI/api-factory-runtime,历史已经搬过来)。工厂自己和 `examples/` 下的项目用 `replace` 编译本仓库的 `runtime/`;
  别处的项目按版本导入。
- **统一版本号**: 一次发布 = 同一个提交上的两个 tag `vX.Y.Z`(工厂)+ `runtime/vX.Y.Z`(运行时模块,`go get` 用的)。
  工厂 vX.Y.Z 生成的项目导入运行时 vX.Y.Z。`factory version` 显示两者;`scripts/release.sh bump vX.Y.Z` 改所有写版本的地方,
  合并后 `scripts/release.sh tag` 打两个 tag。
- **一条命令升级**: `factory upgrade --project DIR` 把项目的运行时依赖(go.mod 的模块路径 + 版本、Go 文件的 import)改成
  这个工厂配套的版本,再 `go mod tidy` + `go build ./...`;之后用同一个工厂重新生成(`factory --project DIR prepare`)。
  `--check` 只检查(CI 用)。
- **性能闸门**(运行时,`.github/workflows/runtime.yml` 的 perf): 写 outbox 的提交吞吐在 16 路并发下 ≥ k × 1 路、
  pg_locks 采样里没有全实例锁的等待、中继从提交到发出的延迟远小于轮询间隔 —— 全是比值,不写死绝对数字。
  见 [runtime/outbox/perf_gate_test.go](runtime/outbox/perf_gate_test.go)。

```sh
factory init ../shop/services/catalog --module github.com/you/shop/services/catalog   # 脚手架: go.mod、buf、cmd/server、factory.yaml
# 写 design/<领域>/ 和 db/schema/*.sql
factory --project ../shop/services/catalog design
factory --project ../shop/services/catalog check
factory --project ../shop/services/catalog prepare        # → proto、gen/、internal/handler、cmd/server/wiring_gen.go、README 的环境变量表
factory system ../shop/services                           # 系统: 跨项目的消息契约检查 + contracts/topology.json(平台在部署前声明)
```

- `--project DIR`: 工厂切换到项目根目录;工厂自己的资源(factory.yaml 的默认值、适配器)仍从工厂目录读(`factory/proj`)
- 项目的 `factory.yaml` 只写项目的键(proto 的包名前缀),按键覆盖工厂的;生成代码的模块名 = 项目 go.mod 的 module
- `examples/` 下的项目(现在: storefront v2 的样例 catalog / inventory / order / payment / notification,见 `docs/design/storefront-v2-readiness.md`)由 `make projects` /
  CI 的 verify ⑤ 检查: 设计 → 生成物是最新的 → 编译 → 用项目自己的数据库验收 → 系统(`factory system --check`)。工厂遇到做不到的,记在 `examples/storefront-v2/GAPS.md`

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
| `insert` / `update` / `delete`(命令) | 工厂(写操作作为数据 → `runtime/txn` 的事务执行器;乐观锁、错误映射) | 无 | CreateJob、DeleteJob、UpdateJobSchedule |
| `command`(组合命令: 条件更新 / 幂等新建 / 多个写操作) | 工厂(操作的列表 → 一个事务;`条件` + 业务原因码、`幂等新建`、`操作名.列`) | 无 | ConsumeJobQuota、StartJobRun、SubmitJobRequest |

映射默认按名字自动对应(`job_type` ↔ `JobType`),对不上时在 `pattern.mapping` 里写。

## 闸门

- **式样书闸门** `factory check` + `buf lint`: 必填项、rpc 是否存在于 proto、read_only 不得写入、fixture 表必须已声明、ai_may_edit 必须在 `internal/handler/` 下
- **路径闸门** `factory guard`: 改动必须在 `ai_may_edit` 内;禁止 import `database/sql`/`pgx`/`net/http`/`os/exec`;禁止 SQL 字面量
- **纯函数闸门**(compute 的逻辑文件): 禁止 `context`/`os`/`net`/`sync`/`math/rand`/平台包;禁止 `time.Now` 等和 goroutine
- **验收** `factory accept`: 按式样书灌 fixture、通过 Connect HTTP/JSON 调用、比对 code 和响应。与运行时语言无关
- **命令的验收**: 每个用例从 fixture 的状态开始,比对 code、响应和 `之后` 的状态;**状态差分测试**: 随机的初始状态 + 随机的请求序列,
  每个请求之后比较响应和表的完整状态,参考 = 内存里的模型状态(`factory/command`,不看 SQL 和生成的代码);
  覆盖条件: 成功 / 不存在 / 版本冲突 / 唯一约束冲突 各 ≥4 次。组合命令: 多表的随机初始状态,每个操作的成功和每种失败(条件不满足、幂等命中 …)各 ≥4 次;
  所有命令另外: **故障注入**(第 k 个写操作之后失败 → 状态 = 请求之前,每个 k)、**并发**(同一个请求同时 8 个: 成功次数 ≤ 串行执行的次数、
  状态 = 串行执行的状态;乐观锁的重读循环没有丢失的更新;幂等新建只有一行)、**重放**(只有幂等新建的命令: 3 次 = 1 次)

## 不交给 AI 的东西(确定性处理)

| 内容 | 由谁处理 |
|---|---|
| 类型、路由、序列化 | buf + protoc-gen-go / connect-go |
| SQL → 查询函数 | 工厂(核心代数的 SQL 后端;逃生口 SQL 用 sqlc 的分析取类型) |
| 参数校验 | protovalidate(规则写在 proto 里) |
| 错误转换、超时、日志 | `platform/` 拦截器 |
| handler 骨架、wiring | `factory prepare` |
| 请求↔查询↔响应的映射 | `factory prepare`(proto 描述符 + gen/db 的 AST) |
| 分页(游标、多取一行、token 签名/绑定) | `list_keyset` 插件 + `runtime/paging` |
| 查询里的"现在"(派生参数的 `现在`) | `runtime/clock`(请求开始的时刻;测试时 `FACTORY_TEST_CLOCK=1` + 请求头 `X-Factory-Now` 固定) |
| 写操作的事务、"没有更新"的原因(不存在 / 版本冲突)、数据库错误的分类、序列化失败的重试 | `runtime/txn`(v0.4.1;READ COMMITTED;条件更新、幂等新建、`Later`;测试用的故障注入 `FACTORY_TEST_FAULTS=1` + `X-Factory-Fail-After`) |
| 写操作失败的原因 → 错误码 | 生成的 `commands_gen.go`(每个服务一个) |

## 模型对照试验

```sh
scripts/trial.sh claude specs/jobs/get-job-summary/spec.yaml 3   # 开发用(快)
scripts/trial.sh local  specs/jobs/get-job-summary/spec.yaml 3   # 最终验证: 本地模型(factory.yaml 的 ai.models.local)
```
结果追加到 `.factory/trials/results.jsonl`,每次的 prompt/diff/闸门/验收输出在 `.factory/trials/<run>/`。

## 许可证

MIT License — 见 [LICENSE](LICENSE)。使用、修改、再发布都可以,请保留版权声明(Copyright (c) 2026 Zaiqi Yang)。
