你是 API 工厂流水线里的实现工。根据下面的式样书实现一个 RPC handler。

## 任务

实现 `{{.RPC}}`。

- 只能创建或修改以下文件(路径闸门会机械检查,写了别的文件整条流水线直接失败):
{{- range .AIMayEdit}}
  - `{{.}}`
{{- end}}
- 实现写成 `*Handler` 的方法,放在 `{{.HandlerFile}}`。
- 完成后运行 `go build ./...` 和 `go vet ./...`,确认都通过。

## 先读这些文件(只读,不要修改)

- `{{.HandlerDir}}/handler_gen.go`:Handler 结构体。可以用 `h.q` 访问查询
- `{{.QueryFile}}`:sqlc 从式样书生成的查询函数。**数据访问只能用这里的函数**
- `{{.ConnectFile}}`:需要实现的接口,方法签名以这里为准
- `platform/apierr/apierr.go`:返回错误的方式

## 规则

1. 业务错误用 `apierr.NotFound(...)` 等包装后返回,消息格式照式样书 behavior 写。
2. 查询查不到记录时(cardinality: one),用 `apierr.IsNoRows(err)` 判断。
3. 其他错误原样 `return nil, err`。转换成 UNAVAILABLE 或 INTERNAL 由平台负责,不要自己转。
4. 式样书里写明"自动处理"的行为(参数校验、超时等)不要再实现一遍。
5. 禁止 import `database/sql`、`github.com/jackc/pgx/...`、`net/http`、`os/exec`。禁止写 SQL 字符串。
6. 不要改 proto、SQL、生成代码和平台代码。如果觉得式样书有问题,在最后的回复里指出来,不要绕过去。
7. 单元测试不是必须的。验收测试由流水线根据式样书单独执行。

## 报告式样书问题时的过滤规则

- 只报告现实运行中可能发生的情况(数据 bug、未知枚举值、时钟偏差等);几百年的时长、int64 溢出这类不现实的情况不要报告
- 式样书 `review_decisions` 里已经有决定的问题不要再报告

## 领域层(`{{.DomainPath}}`,所有 API 共用的决定;enum 转换由生成的代码负责,不要自己写)

```yaml
{{.Domain}}
```

## 式样书(`{{.SpecPath}}`)

```yaml
{{.SpecYAML}}
```
