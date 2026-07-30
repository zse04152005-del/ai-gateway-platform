# openaiadapter

`openaiadapter` 是注册类型为 `openai` 的官方 OpenAI Chat Completions HTTP 适配器。它不依赖供应商 SDK，不硬编码“最新模型”，实际 `model` 始终来自经过目录校验的 Deployment `PhysicalModel`。

## 安全边界

- 正式 Endpoint 必须使用 HTTPS，且只能是根路径、`/v1` 或 `/v1/chat/completions`；UserInfo、Query、Fragment 和其他 Path 在 Factory 阶段拒绝。
- 数字 Loopback HTTP 仅在 `AllowInsecureLoopback` 被测试代码显式启用时可用，生产配置不会隐式降级。
- Deployment 必须绑定同 Provider 的 Secret Reference。`BuildRequest` 在请求构造的最后边界通过 `providersecret.Locator` 解析凭据，将调用方拥有的 `[]byte` 写入 Authorization 后立即清零；Factory、Registry、错误、Usage、日志结构和 Fixture 都不保存明文 Key。
- Secret Resolver 的私有错误不会向上透传，只返回稳定的 `ErrCredentialUnavailable`。
- 请求最大 1 MiB、普通响应最大 1 MiB、错误响应读取最大 64 KiB、SSE 单行最大 64 KiB、单事件最大 256 KiB。
- Provider 原始错误正文不会进入 `NormalizedError`；只使用 HTTP 状态、合法的 `X-Request-ID` 和有界 `Retry-After`。

## 当前协议映射

- 请求：messages、工具与 Tool Choice、Response Format、temperature、top_p、stop、`max_completion_tokens`。
- 流式请求强制发送 `stream_options.include_usage=true`。
- 普通响应：assistant 文本、function tool calls、有限 Finish Reason、Provider Request ID。
- Usage：`prompt_tokens`、`completion_tokens`、`prompt_tokens_details.cached_tokens`、`cache_write_tokens`、输入音频 Token、`completion_tokens_details.reasoning_tokens` 与输出音频 Token。
- 未映射 Usage 字段以排序 JSON Pointer 和精确原始 JSON 的 SHA-256 Evidence 保留，不猜测计费语义。
- SSE：支持官方 `chat.completion.chunk`、空 `choices` 的最终 Usage Chunk 和 `[DONE]`；终止前缺少 Usage 时明确输出 `usage_status=missing`。
- 官方无语义影响的 `service_tier`、`system_fingerprint` 和 `obfuscation` 被识别；真正未知 SSE 字段隔离为 `provider_extension`，未知普通响应字段 fail closed。
- 401/403/408/429/5xx 等状态映射到有限的 Provider-neutral Error Category；仅可重试类别读取 `Retry-After`。

## 显式不支持项

当前 Normalized Protocol 无法无损表达下列官方能力，因此适配器明确拒绝或以协议错误关闭，不会静默丢弃：

- 音频输入/输出正文；仅 Usage 中的音频 Token 计量可保留。
- 并行工具调用的独立策略开关；目录可声明能力，但本版本不会自动开启。
- Refusal、Annotations/Citations、Legacy `function_call`、Logprobs 和 Moderated Completion Payload。
- 任意 Provider Options 透传及供应商特定 Policy Label。
- 本地 Token 估算；`EstimateUsage` 返回 `ErrUsageEstimationUnavailable`，避免把估算当成供应商账单。

## 离线一致性验收

`conformance_test.go` 通过真实 Loopback HTTP Fixture 运行 `adapterconformance.Run` 的完整必选矩阵，不需要外网或 OpenAI Key。Fixture 使用官方 Chat Completions JSON/SSE 结构，特别验证 Finish Chunk、空 Choices Usage Chunk、`[DONE]` 的顺序。

## 官方协议依据

- [Chat Completions Overview](https://developers.openai.com/api/reference/chat-completions/overview.md)
- [Chat endpoint reference](https://developers.openai.com/api/reference/resources/chat.md)
- [Chat Completions streaming events](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events.md)
- [OpenAI API error codes](https://developers.openai.com/api/docs/guides/error-codes.md)

官方文档建议新项目优先评估 Responses API；本适配器保留 Chat Completions 是为了兼容当前网关的 OpenAI-compatible 客户端面和既定 Adapter Contract。后续若增加 Responses Adapter，应使用新的注册类型和独立一致性 Fixture，不在此实现中隐式切换协议。
