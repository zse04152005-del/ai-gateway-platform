# Non-stream Proxy

`NonStreamExecutor` 接受已经授权并选择完成的 `routing.Selection`，只执行一次 Provider Attempt，不自行重新选路、重试或拼接响应。它通过显式 Registry 构建 Deployment-scoped Adapter，调用进程级共享 `upstreamhttp.Client`，并让 Adapter 在有界读取后产生 `NormalizedResponse` 或 `NormalizedError`。

错误边界：

- Adapter 创建/请求构造失败归为 `ErrAdapterUnavailable`。
- 没有可用 HTTP 响应归为 `ErrTransport`，同时保留 Context/Timeout 的 `errors.Is` 分类。
- Provider 非 2xx 只保留已验证的 `ProviderError.Detail()`；原始 Body 不进入该结构。
- 格式、媒体类型、模型身份和 Adapter 协议违规归为 `ErrProtocol`。
- `executionError.Error()` 只返回稳定类别，内部 cause 仅供可信控制流使用。
- 入站取消沿同一 Context 进入上游 `http.Request`；正在读取响应 Body 时取消会释放真实 HTTP Handler/连接，并保留 `errors.Is(context.Canceled)`。

Gateway 在本执行边界外创建并迁移 Request/Attempt；P08 在每次调用 `Execute` 之前决定重试与故障切换，Executor 本身保持“一个 Selection 等于一个 Attempt”。
