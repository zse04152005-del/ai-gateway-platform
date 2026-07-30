# Provider Adapter 契约

> 状态：Accepted for MVP；Normalized Types 已实现
> 日期：2026-07-30  
> 对应任务：P01-T08

## 1. 目标

Provider Adapter 只负责供应商协议与统一领域协议之间的转换，不负责租户认证、预算、路由选择、重试策略和长期计费。

P05-T02 的可执行类型、不变量、原始 Usage 证据和安全日志规则见 [`normalized-provider-types.md`](./normalized-provider-types.md) 与 `internal/adapter`；本文件中的接口将在 P05-T03 注册表任务落地。

## 2. 已实现 Go 接口

```go
type Adapter interface {
    Type() Type
    Capabilities(ctx context.Context) CapabilitySet
    BuildRequest(ctx context.Context, in adapter.NormalizedRequest) (*http.Request, error)
    ParseResponse(ctx context.Context, resp *http.Response) (adapter.NormalizedResponse, error)
    OpenStream(ctx context.Context, resp *http.Response) (ChunkStream, error)
    NormalizeError(ctx context.Context, resp *http.Response, body []byte) adapter.NormalizedError
    EstimateUsage(ctx context.Context, in adapter.NormalizedRequest) (adapter.NormalizedUsage, error)
}

type ChunkStream interface {
    Next(ctx context.Context) (adapter.NormalizedChunk, error)
    Close() error
}
```

实际接口位于 `internal/provideradapter`；`Factory.New` 将已经过校验的 Catalog Provider/Deployment 绑定为一个 Adapter，注册与启动/发布门禁见 [`provider-adapter-registry.md`](./provider-adapter-registry.md)。

## 3. NormalizedRequest

至少包含：

- requestId、logicalModel、messages。
- stream、temperature、topP、maxOutputTokens、stop。
- tools、toolChoice、responseFormat。
- tenant 策略产生的内容分类标签，但不包含供应商密钥。
- providerOptions：只允许经过 Deployment Schema 验证的扩展。

Adapter 不得静默丢弃影响结果、工具或费用的字段。无法表达时返回 `unsupported_parameter`。

## 4. NormalizedChunk

```text
sequence
kind: message_start | content_delta | reasoning_delta | tool_delta |
      usage_delta | message_end | heartbeat | provider_extension
content/tool delta
finish_reason
usage_delta
provider_event_type
observed_at
```

- heartbeat 不进入模型内容。
- provider_extension 必须有限大小，默认不向客户端透传。
- message_end 缺少 usage 时标记 `usage_status=missing`，不得当作 0。

## 5. NormalizedUsage

| 字段 | 说明 |
|---|---|
| inputTokens | 普通输入 Token |
| outputTokens | 普通输出 Token |
| cacheReadTokens | Prompt Cache 读取 Token |
| cacheWriteTokens | Prompt Cache 写入 Token |
| reasoningTokens | 推理/思考 Token（供应商支持时） |
| audioInput/Output | 音频计量扩展 |
| source | provider、estimated、reconciled、adjustment |
| complete | 是否为最终完整 Usage |
| rawEvidenceHash | 原始计量证据摘要，不存敏感正文 |

未知计费类型写入扩展并触发可观测告警；在价格映射明确前不得按 0 结算。

## 6. NormalizedError

```text
code
category: auth | permission | invalid_request | rate_limit | capacity |
          timeout | provider_5xx | content_policy | context_length |
          protocol | cancelled | unknown
retryable
retry_after
provider_status
safe_message
provider_request_id
```

重试策略由 Gateway 根据 category、首包状态、总时间和 Attempt 上限决定；Adapter 只提供规范化事实。

## 7. 能力集合

- chat、stream、tools、parallelTools、structuredOutput、vision。
- maxContextTokens、maxOutputTokens。
- usageInStream、cacheUsage、reasoningUsage。
- region、dataRetentionMode、providerProtocolVersion。

Route Candidate 必须先满足请求所需能力。

## 8. 一致性测试矩阵

每个 Adapter 至少声明 Applicable/Not Applicable，并执行：

1. 非流式成功与 usage。
2. SSE 正常结束、Chunk 跨网络分片、`[DONE]`。
3. 首包前 429/5xx/超时。
4. 首包后断流与部分 usage。
5. 客户端取消。
6. 缓存读/写 Token。
7. Tool Call 与结构化输出。
8. 上下文过长、认证和内容策略错误。
9. 未知字段、错误 JSON、超大 Chunk。
10. Provider 协议版本变化。

## 9. 安全要求

- BuildRequest 只能访问已批准 Endpoint 和密钥引用解析结果。
- 不能把虚拟 Key 转发给 Provider。
- Provider 响应 Header 默认白名单透传。
- 错误与 Trace 不含 Provider Key、Prompt/Response 或完整原始 Body。
- Parser 对正文、行和嵌套深度设置上限。
