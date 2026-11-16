你是 API 工厂流水线里的实现工。根据下面的式样书,实现一个**纯函数**。

## 任务

在 `{{.LogicFile}}` 里实现下面这个函数(文件里已经有一个返回 UNIMPLEMENTED 的桩,把它替换掉):

```go
{{.Signature}}
```

- 调用它的 handler 已经由工厂生成(`{{.HandlerGenFile}}`),不要修改。请求参数校验、查询、错误转换都已经处理好了。
- 只能创建或修改以下文件(路径闸门会机械检查,写了别的文件整条流水线直接失败):
{{- range .AIMayEdit}}
  - `{{.}}`
{{- end}}
- 完成后运行 `go build ./...` 和 `go vet ./...`,确认都通过。如果写了测试,也运行 `go test ./internal/...`。

## 输入和输出的类型(只读,不要修改)

- `req`:请求,类型定义在 `{{.PBFile}}`
- `rows`:查询结果,类型定义在 `{{.QueryFile}}`。可空列是指针(`*time.Time` 等),nil 表示 NULL
- 返回值:响应,类型定义在 `{{.PBFile}}`。`optional` 字段是指针,"不设置"就保持 nil
- 时间转换:`*timestamppb.Timestamp` 用 `.AsTime()` 转成 `time.Time`

## 纯函数的规则(闸门会检查)

1. 不访问 DB、网络、文件,不 import `context`、`os`、`net`、`sync`、`math/rand`、`apifactory/platform/...`
2. 不调用 `time.Now` / `time.Since` 等依赖当前时间的函数,不启动 goroutine
3. 不使用全局可变状态
4. 业务上无法计算的情况,照式样书 behavior 处理;式样书没写的情况,在最后的回复里指出来,不要自作主张

## 报告式样书问题时的过滤规则

- 只报告现实运行中可能发生的情况(数据 bug、未知枚举值、时钟偏差等);几百年的时长、int64 溢出这类不现实的情况不要报告
- 式样书 `review_decisions` 里已经有决定的问题不要再报告

## 式样书(`{{.SpecPath}}`)

```yaml
{{.SpecYAML}}
```
