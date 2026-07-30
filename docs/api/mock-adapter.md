# Mock Adapter 协议转换与流状态机

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P05-T04
>
> 实现：`internal/mockadapter`

## 1. 目标

Mock Adapter 不是在测试中直接构造 `NormalizedResponse` 的捷径，而是第一个完整执行 Provider Adapter 契约的实现：

```text
NormalizedRequest
  -> BuildRequest
  -> real HTTP /v1/chat/completions
  -> Mock Provider JSON/SSE/Error
  -> ParseResponse/OpenStream/NormalizeError
  -> NormalizedResponse/Chunk/Error/Usage
```

测试通过共享 `httpserver` 生命周期和真实 `httptest.Server` 发送 HTTP，请求必须经过 JSON 编码、HTTP Transport、响应大小限制、SSE 分片和 Body 关闭；不得按测试名直接返回内存 Fixture。

## 2. 注册与本地安全边界

Factory Type 固定为 `mock`，通过 P05-T03 的不可变 Registry 显式注册。Factory 只接受：

- Provider/Deployment 都为 `active`；
- Provider `adapter_type=mock`；
- Deployment 属于该 Provider 且声明 Chat；
- Endpoint Scheme 为 `http`；
- Host 必须是显式数字 Loopback IP，不接受 `localhost` 或其他 Hostname；
- Path 只能为空、`/v1` 或 `/v1/chat/completions`；
- 不允许 `secret_reference_id`。

这样 Mock Adapter 不会被误配置为真实生产降级通道，也不会把 Provider Key 带到本地模拟服务。Redirect/通用 SSRF/Egress 策略仍由 P12 的共享 HTTP Client 层执行。

## 3. 请求映射

`BuildRequest` 完整映射：

| Normalized 字段 | Mock/OpenAI-compatible 字段 |
|---|---|
| Logical Model | 不转发；使用已绑定 Deployment Physical Model |
| Text/Media Parts | String 或 typed content array |
| Assistant Tool Calls | `tool_calls[].function` |
| Tool Result | `role=tool` + `tool_call_id` |
| Temperature/TopP/MaxOutput | `temperature/top_p/max_tokens` |
| Stop | `stop` |
| Tool Definition/Choice | Function Tool / string 或 named object |
| Response Format | `text/json_object/json_schema` |
| Stream | `stream` + `Accept: text/event-stream` |
| Request ID | `X-Request-ID` |

Adapter 不添加 `Authorization`。编码后的请求最大 1 MiB，与 Mock Provider 入口一致。

### 3.1 Mock Provider Options

```json
{
  "mock_scenario": "cached-usage",
  "mock_delay_ms": 100
}
```

- 只允许这两个字段，未知字段失败。
- `mock_delay_ms` 仅用于 `delay`，范围 1～5000。
- `sse`、`malformed-chunk` 要求 `stream=true`。
- `normal`、`fixed-usage`、`cached-usage`、`tool-call` 是非流式场景，不能与 `stream=true` 混用。
- 429、503、disconnect 可用于首包/协议错误验证。

`PolicyLabels` 当前没有 Mock Provider 协议映射，因此非空时返回 `ErrUnsupportedParameter`。这是有意的 fail closed，避免影响安全或结果的字段被静默丢弃。

## 4. 普通响应

Parser：

1. 接管并关闭 Body；
2. 限制为 1 MiB；
3. 要求 HTTP 200 + `application/json`；
4. 白名单校验顶层、Choice、Message、Tool Call 和 Function 字段；
5. 校验 `object=chat.completion` 与实际 Physical Model；
6. 映射 Assistant Content、Tool Calls 和 Finish Reason；
7. 解析 Usage 原始 JSON；
8. 对最终 NormalizedResponse 再运行领域 `Validate`。

普通响应出现未知结构字段会返回安全 `ProtocolError`，不会静默忽略协议漂移。Finish Reason 新值归类为 `unknown` 并保留 `providerFinishReason`。

## 5. Usage 映射

| Provider 字段 | Normalized 字段 |
|---|---|
| `prompt_tokens` | Input Tokens |
| `completion_tokens` | Output Tokens |
| `prompt_tokens_details.cached_tokens` | Cache Read Tokens |
| `completion_tokens_details.reasoning_tokens` | Reasoning Tokens |
| `total_tokens` | 完整性校验，不作为额外计费维度重复相加 |

Provider 报告的 0 使用 `Present=true`；字段未出现保持 Missing。`total_tokens` 必须等于 Input + Output。

Usage 对象中的未知顶层或 Detail 字段不会丢失：Adapter 保存完整 Raw Evidence、SHA-256，并把字段位置写成排序 JSON Pointer，例如：

```text
/future_meter
/prompt_tokens_details/future_cache_class
```

后续 Pricing 在映射明确前不能把这些 Meter 按 0 结算。

## 6. SSE 状态机

Mock Provider 的正常 SSE 顺序是：

```text
role -> content* -> finish -> usage -> [DONE]
```

Normalized 状态机：

```text
message_start -> content/reasoning/tool delta* -> message_end(usage status)
```

关键处理：

- 收到 Finish 时先暂存，不立即把 Usage 标成 Missing；
- 后续收到 Usage 时，把完整 Usage 附加到 `message_end`，状态为 `present`；
- 若直到 `[DONE]` 仍没有 Usage，才输出 `message_end + missing`；
- Usage 先于 Finish 时同样暂存，等待 Finish；
- 重复 Finish、重复 Usage、`[DONE]` 前没有 Finish、结束后继续发事件或 EOF 前没有 `[DONE]` 都是协议错误；
- SSE Comment 映射 Heartbeat，永不进入模型内容；
- 未知 Chunk 字段以最多 16 KiB 的原始事件隔离成 `provider_extension`，默认不向客户端透传；
- Sequence 从 0 单调递增，Chunk 构造后立即执行领域 Validate。

单行限制 64 KiB、完整 Event 限制 256 KiB。`Next(ctx)` 使用 `context.AfterFunc` 在取消时关闭 Body，从阻塞读取中退出；这同时触发上游请求 Context 取消。

## 7. 错误语义

| HTTP/事实 | Category | Retryable | Code |
|---|---|---:|---|
| 400/404/405/413/415 | invalid_request | false | `MOCK_INVALID_REQUEST` |
| 401 | auth | false | `MOCK_AUTH_FAILED` |
| 403 | permission | false | `MOCK_PERMISSION_DENIED` |
| 408/504 | timeout | true | `MOCK_PROVIDER_TIMEOUT` |
| 429 | rate_limit | true | `MOCK_RATE_LIMITED` |
| 503 Mock capacity | capacity | true | `MOCK_PROVIDER_UNAVAILABLE` |
| 其他 5xx | provider_5xx | true | `MOCK_PROVIDER_FAILED` |
| 未识别状态 | unknown | false | `MOCK_PROVIDER_ERROR` |
| Context 已取消 | cancelled | false | `UPSTREAM_CANCELLED` |

Retry-After 只接受正整数秒或有效 HTTP Date，且必须在 24 小时内。Provider Error Message 和 Raw Body 不进入 NormalizedError；内部 JSON/网络 cause 只通过安全 ProtocolError 的 `Unwrap` 提供给受控诊断。

## 8. 自动化验收

真实 HTTP 测试覆盖：

- normal、fixed-usage、cached-usage、tool-call；
- SSE Role、Content、Reasoning、Tool Delta、Usage、Finish、Heartbeat、`[DONE]`；
- delay 的普通与流式模式；
- 429、503 和完整错误分类/Retry-After 矩阵；
- disconnect 半包、malformed chunk、无 `[DONE]`、超大响应/行、错误 Content-Type；
- 未知普通字段 fail closed、未知 Stream 字段隔离、未知 Usage 字段保留；
- Request Options/Stream 冲突、Policy Label、超大请求和不安全 Endpoint；
- Context 取消使阻塞 `Next` 退出并取消上游；
- Registry 启动校验和 Factory Build 的真实组合路径。

核心包执行 20 轮重复测试并在 Linux CI 使用 race detector。P05-T05 已把跨 Adapter 的普通/SSE/取消/错误/缓存/工具/Finish/未知字段断言提取到 [`Adapter Conformance Suite`](./adapter-conformance-suite.md)；本文件中的大小限制、状态机异常和 Mock Options 等协议专项测试继续保留，避免统一套件稀释实现边界。
