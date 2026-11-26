# 试一下改动(`factory try`)

`factory [--project DIR] try --changes FILE [--api ID]... [--base REV] [--out FILE] [--keep]` 回答一个问题:
**这些文件改了以后,工厂还接受这个项目吗?哪些用例通过、哪些失败?** 结果是 JSON(`factory.try-result/v1`)。
给工厂以外的工具用,和 [`factory export`](api-manifest.md) 配套: 清单告诉外部工具该看哪里,`try` 告诉它改了以后会怎样。

## 为什么要有 try

外部工具(文档一致性检查等)想知道的两件事,工厂自己的流水线回答不了:

- **例子守不守得住规则**: 故意改坏一个逃生层的文件(变异,例如把 SLA 的 300 秒改成 301 秒),看设计书的例子能不能抓到。
  抓不到 = 这条规则只写在文字里,没有例子守着。
- **修改提案是不是成立**: 设计书的修改(加一个例子、改一个期待值、改写规则的文字)提出之前,先证明它仍然通过验收。
  例子写的和现在的行为不一样,`try` 会指出是哪一个用例。

所以 `try` 只是把工厂已有的步骤(设计 → 确定性生成 → 编译 → 单元测试 → 验收)放在一个**隔离的副本**里跑,
把结果按用例、按来源输出。它不调用 AI、不访问网络(除了照常拉数据库的镜像),同样的输入总是同样的结果。

## 怎么跑

1. 基准的修订(`--base`,默认 `HEAD`)用 `git archive` 展开到临时目录。调用者的工作区、索引、`.git` 都不动;
   没有提交的改动不会带进去(要试的东西写进改动的清单)。结束后删除临时目录(`--keep` 留下,路径打印到标准错误)。
2. 在副本里应用改动的清单。
3. **设计**: `design/` → proto + specs(`factory design`)。失败 = `gate_failed`。
4. **需要 AI 吗**: 逃生层的主文件(计算的逻辑文件、`custom` 的 handler)还没写 → `needs_ai`,不调用 AI。
5. **生成**: `factory prepare`(式样书闸门在里面)。失败 = `gate_failed`。
6. **选 API**: `--api` 指定的;没有指定 = 改动涉及的 API(用 `factory export` 的清单: 来源、式样书、生成的文件、
   逃生层、验收文件;`design/<领域>/` 里不属于某个 API 的文件 = 这个领域的全部 API);一个都不涉及 = 全部。
7. **编译**: `go build ./cmd/server` + `go build ./...`。失败 = `build_failed`。
8. **单元测试**: `go test -json ./internal/...`。测试编译不过 = `build_failed`(`stage: test`)。
9. **验收**: 自己的 Postgres 容器(名字 `factory-try-<进程号>-<随机>`,端口由 docker 分配,标签 `factory-try=1`),
   镜像和初始化参数和 `make db-up` 一样(`PG_IMAGE` / `IMAGE_MIRROR`)。开发者的 `api-factory-pg` 和并行的 `try`
   都不会撞。选中的 API 并行验收(每个 worker 一套数据库;工厂仓库自己另有两个分库,和 Makefile 一样)。结束时删除容器。

调用者环境里指向开发者的数据库 / 消息 / 服务器的变量(`DATABASE_URL`、`FACTORY_SHARDS`、`AMQP_URL`、`FACTORY_DIFF_SEED` 等)
在副本里不用。验收用的是默认的差分种子。`try` 用的是正在运行的这个工厂(不是副本里的工厂源码)。

## 改动的清单(`--changes`,`-` = 标准输入)

```json
{"schema": "factory.try-request/v1",
 "files": [{"path": "design/jobs/GetJobSummary.yaml", "content": "…新的全文…"},
           {"path": "internal/handler/jobs/get_job_summary_logic.go", "content": "…"},
           {"path": "some/file", "delete": true}]}
```

| 字段 | 意思 |
|---|---|
| `path` | 相对项目根目录,斜杠分隔。绝对路径、跑出项目的路径、`.git` 下的路径不行;同一个路径不能写两次 |
| `content` | 新的全文(不是差分)。空字符串 = 空文件 |
| `delete` | `true` = 删除。和 `content` 只能有一个;基准里没有这个文件 = 用法错误 |

不认识的字段 = 用法错误(清单的格式由 `schema` 决定)。

## 结果

```json
{"schema": "factory.try-result/v1",
 "base": "6157085…(完整的提交)",
 "status": "ok",
 "stage": "accept",
 "message": "",
 "apis": [{"id": "jobs.get-job-summary",
           "cases": [{"name": "mu — 300.5 秒算违约", "source": "example", "passed": false,
                      "detail": "response.health: want DEGRADED, got OK (body …)"},
                     {"name": "Test_getJobSummaryLogic_Examples/…", "source": "logic_example", "passed": true}]}],
 "summary": {"passed": 39, "failed": 4,
             "by_source": {"example": {"passed": 15, "failed": 2}, "auto": {"passed": 11, "failed": 0},
                           "differential": {"passed": 0, "failed": 0},
                           "logic_example": {"passed": 4, "failed": 0}, "hand_test": {"passed": 9, "failed": 2}}}}
```

| 字段 | 意思 |
|---|---|
| `base` | 基准的提交(完整的 sha) |
| `status` | `ok` = 跑完了(用例可能有失败);`gate_failed` = 设计 / 式样书闸门不通过;`build_failed` = 编译不通过(服务或测试);`needs_ai` = 生成需要 AI;`error` = 基础设施的错误 |
| `stage` | 停在哪个阶段: `design` / `prepare` / `build` / `test` / `accept`(跑完 = `accept`) |
| `message` | 不是 `ok` 时: 闸门 / 编译的错误(最后 40 行)、需要 AI 的文件。`ok` 时: 选中的 API 以外失败的单元测试、没能执行的验收(否则为空) |
| `apis` | 选中的 API(排好序)。没跑到测试就停了 = `cases` 为空(= 没执行,不是没抓到) |
| `apis[].error` | 这个 API 的验收没能执行(例如服务器启动不了);`cases` 只有单元测试 |
| `cases[].source` | `example` = 设计书的 `例子.用例` / 式样书的 `acceptance.cases`(人写的例子);`auto` = 自动生成的验收用例;`differential` = 差分用例;`logic_example` = 从 `逻辑样例` 生成的单元测试(`*_logic_examples_gen_test.go`);`hand_test` = 逃生层里手写的单元测试 |
| `cases[].name` | 验收用例 = 用例的说明;单元测试 = `go test` 的名字(子测试一个一项,名字里的空格是 `_`) |
| `cases[].detail` | 失败的第一行(通过的用例没有) |
| `summary` | 全部 `cases` 的计数;`by_source` 总是列出五种来源 |

通过的用例也列出来: 调用者能区分"抓到了"(有失败)、"没抓到"(全部通过)和"没执行"(没有用例)。
不属于选中 API 的单元测试(enum 的生成测试等)不算进用例;失败了写在 `message` 里。

## 退出码

| 退出码 | 什么时候 |
|---|---|
| 0 | 跑出了结论: `ok`(包括用例失败)、`gate_failed`、`build_failed`、`needs_ai` |
| 1 | 基础设施的错误(docker 没有、git 失败等): 结果照样输出,`status: error` |
| 2 | 用法错误(选项、改动的清单、不存在的 `--api`、不是提交的 `--base`): 没有结果,原因在标准错误 |

结果写到标准输出,`--out FILE` 写到文件(权限 0600)。进度(每个阶段的时间)写到标准错误。

格式的版本: 只加字段时不改版本;改字段的意思或删字段时升级 `schema`。
