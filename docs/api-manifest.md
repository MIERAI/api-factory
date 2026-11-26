# API 清单(`factory export`)

`factory [--project DIR] export [--out FILE]` 把项目的 API 输出成 JSON(`factory.api-manifest/v1`)。
给工厂以外的工具用: 文档一致性检查、影响分析、评审范围的计算等。

## 为什么要有清单

工厂保证的是**设计书 → 代码**这个方向: 生成是确定的,闸门检查生成物是最新的,验收从设计书的例子生成。
所以生成的部分不需要外部工具再检查,外部工具也不应该去解析生成物(生成物的写法会随工厂的版本变)。

工厂保证不了的是**逃生层**: 计算的逻辑函数、`custom` 的 handler、`datasource.sql`。这些代码改了,设计书的
文字(业务规则、说明)不会跟着改;例子覆盖的行为由验收守住,例子没覆盖的规则没人守。清单告诉外部工具
**该看哪里**: 哪些规则、对应哪些手写的文件。

## 格式

```json
{
  "schema": "factory.api-manifest/v1",
  "adapter": "go",
  "project": ".",
  "apis": [{
    "id": "jobs.get-job-summary",
    "rpc": "factory.jobs.v1.JobService/GetJobSummary",
    "name": "GetJobSummary", "domain": "jobs", "kind": "compute",
    "source": {"path": "design/jobs/GetJobSummary.yaml", "line": 0},
    "source_kind": "design",
    "spec": "specs/jobs/get-job-summary/spec.yaml",
    "rules":    [{"path": "design/jobs/GetJobSummary.yaml", "line": 35, "when": "…", "then": "…"}],
    "examples": [{"path": "design/jobs/GetJobSummary.yaml", "line": 85, "name": "…", "kind": "acceptance"}],
    "generated": ["db/queries/jobs.get-job-summary.sql", "…"],
    "escape": [{"path": "internal/handler/jobs/get_job_summary_logic.go", "line": 0, "kind": "logic", "exists": true}],
    "acceptance": "gen/acceptance/jobs.get-job-summary.yaml"
  }],
  "hand_files": ["cmd/server/main.go", "…"]
}
```

| 字段 | 意思 |
|---|---|
| `project` | 项目根目录(相对工厂的根目录)。其他路径都相对它 |
| `source` / `source_kind` | 人写的来源: `design` = `design/<领域>/<API>.yaml`;`spec` = 手写的式样书 |
| `rules` | 业务规则(设计书的 `业务规则` / 式样书的 `behavior`),行号指向规则那一项 |
| `examples` | 例子(`acceptance` = 验收用例,`logic` = 逻辑样例)。例子由工厂执行,它描述的行为由验收保证 |
| `generated` | 文件头写着从 `source` 或 `spec` 生成的文件 |
| `escape` | 逃生层: `logic`(计算的纯函数)/ `handler`(`custom`)/ `test` / `sql`(`datasource.sql`,行号指向 `sql:`)。`exists: false` = 允许写但还没写 |
| `hand_files` | 项目里所有手写的服务代码(和 `factory stats` 同一个定义) |

清单是确定的: 同一个提交总是同样的输出(不含时间、不含绝对路径)。只加字段时不改版本;改字段的意思或删字段时升级 `schema`。
