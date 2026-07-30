# correlation

本包实现服务共享的请求关联边界：

- 接受 8～128 字符的安全 `X-Request-Id`；非法、多值、并发冲突或近期重放时生成 128-bit 随机 `req_...` ID。
- 活跃 ID 永不因 TTL 提前释放；完成后的 ID 进入有容量上限的近期冲突窗口，防止无界内存增长。
- 严格解析 W3C `traceparent` v00；无效父上下文生成新 128-bit Trace ID，每个服务请求始终生成新的 64-bit Span ID。
- 仅在有效父上下文存在时接受经过边界校验的 `tracestate`，并限制为 512 字节、32 个成员且禁止重复键。
- 响应回传 `X-Request-Id` 和 `traceparent`；`InjectHTTP` 将相同 Request/Trace 关联注入下游 HTTP 请求。
- ID 熵源失败时返回统一安全错误，内部随机源错误不会出现在响应中。

中间件只建立关联上下文，不记录 Prompt/Response，也不把客户端 ID 当作租户或授权身份。
