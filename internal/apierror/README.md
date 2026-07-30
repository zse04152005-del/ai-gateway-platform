# apierror

`apierror` 是所有 HTTP 进程共享的错误边界：

- `Definition` 只保存经过校验、可公开且稳定的状态码、错误码、消息、类型、参数和重试提示。
- `Error` 私有保存内部 `cause`，支持 `errors.Is`/`errors.As`，但 HTTP Renderer 永不序列化 `Error()` 或 cause。
- 未分类错误统一降为安全的 `500 INTERNAL_ERROR`，不会把堆栈、内部地址、Provider Body 或凭据返回给客户端。
- Request ID 在传输边界渲染时注入，避免领域错误持有请求生命周期状态。
- 调用方必须使用固定公开消息，动态诊断内容只能作为 cause 或结构化安全日志属性。

依赖方向：transport/application 可以依赖本包；本包只依赖 Go 标准库，不依赖领域、数据库或供应商 SDK。
