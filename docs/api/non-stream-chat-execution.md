# 非流式 Chat 执行与统一响应

状态：P06-T07 已实现本地代码与自动化验证。

## 1. 执行链

```text
Virtual Key authentication
  -> strict request parsing
  -> NormalizedRequest
  -> tenant/project/key-scoped Selection
  -> deployment-scoped Adapter
  -> shared upstream HTTP Client
  -> Adapter.ParseResponse
  -> NormalizedResponse / NormalizedError
  -> unified client JSON / public API error
```

`NonStreamExecutor` 一次只执行一个已经选定的 Deployment，不在内部重选、重试或跨模型拼接。Gateway 已把一次 `Execute` 精确对应为一个持久化 Route Attempt，P08 再在外层基于安全分类决定是否重试或故障切换。

Gateway 进程显式注册 `mock` 和 `openai` 两个 Adapter Factory。开发环境可以通过 PostgreSQL Provider Secret Reference + 本地 Envelope Resolver 调用 OpenAI；没有本地 Key 的环境仍可启动和使用 Mock，但 OpenAI 凭据解析会 fail closed。正式 Vault/KMS Resolver 在 P12-T03 接入。

## 2. 成功响应

客户端看到稳定逻辑模型，不看到物理模型、Deployment、Endpoint、Provider Request ID、Secret Reference 或路由内部错误：

```json
{
  "id": "chatcmpl_fixture",
  "object": "chat.completion",
  "created": 1785412800,
  "model": "general-chat",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": null,
        "tool_calls": [
          {
            "id": "call_weather",
            "type": "function",
            "function": {
              "name": "get_weather",
              "arguments": "{\"city\":\"Shanghai\"}"
            }
          }
        ]
      },
      "finish_reason": "tool_calls"
    }
  ],
  "usage": {
    "prompt_tokens": 13,
    "completion_tokens": 3,
    "total_tokens": 16,
    "prompt_tokens_details": {"cached_tokens": 5},
    "completion_tokens_details": {"reasoning_tokens": 2},
    "source": "provider"
  },
  "gateway": {
    "request_id": "req_fixture",
    "attempt_count": 1,
    "usage_complete": true
  }
}
```

## 3. 字段映射

| Normalized 字段 | 公共字段 | 规则 |
|---|---|---|
| ResponseID | `id` | Adapter 已验证的安全标识 |
| ObservedAt | `created` | UTC 观测时间的 Unix 秒 |
| 请求 LogicalModel | `model` | 永远不替换为物理模型 |
| Message text parts | `message.content` | 按顺序拼接；无文本且有 Tool Call 时为 `null` |
| ToolCall | `tool_calls[].function` | Arguments 作为已验证 JSON 对象的字符串输出 |
| FinishStop | `stop` | 正常完成 |
| FinishLength | `length` | 输出/上下文限制 |
| FinishToolCalls | `tool_calls` | 模型请求工具 |
| FinishContentPolicy | `content_filter` | 统一兼容名 |
| FinishCancelled/Error/Unknown | `cancelled`/`error`/`unknown` | 不回显供应商原始原因 |

Provider Usage 只有在 Input/Output Token 都明确 `Present` 时才进入公共 `usage`；不能把缺失事实伪装成 0。`total_tokens` 由已验证的 Input + Output 计算，Cache Read/Write、Reasoning、Audio Input/Output 只在各自存在时输出。`gateway.usage_complete=false` 明确表示 Usage 缺失或不完整；完整 Normalized Usage 仍交给后续 P10 账本，公共投影不会销毁内部证据。

## 4. 错误映射

| 内部分类 | HTTP | 公共 Code | Retryable |
|---|---:|---|---|
| Provider rate limit | 429 | `PROVIDER_RATE_LIMITED` | 继承已验证值 |
| Timeout / deadline | 504 | `PROVIDER_TIMEOUT` | 是 |
| Capacity / Provider 5xx | 503 | `PROVIDER_UNAVAILABLE` | 继承已验证值 |
| Provider credential/permission | 502 | `PROVIDER_CREDENTIAL_ERROR` | 否 |
| Provider rejected normalized request | 502 | `PROVIDER_REQUEST_REJECTED` | 否 |
| Content policy | 403 | `CONTENT_POLICY_REJECTED` | 否 |
| HTTP connection failure | 502 | `PROVIDER_CONNECTION_FAILED` | 是 |
| Protocol/response validation | 502 | `PROVIDER_PROTOCOL_ERROR` | 否 |
| Client cancellation | 499 | `REQUEST_CANCELLED` | 否 |

Provider Body、Provider message、Endpoint、数据库错误和 Adapter 私有 cause 不进入错误 Envelope。`Retry-After` 只接受 Adapter 已校验的正时长且最多 24 小时。

## 5. 取消传播

Inbound HTTP Request、Gateway Handler、`NonStreamExecutor`、Adapter 构造/解析和上游 `http.Request` 使用同一条 Context 取消链。客户端断开或主动取消时：

- 正在等待响应头或读取响应 Body 的 Provider 调用立即解除阻塞并释放服务端 Request Context；
- Executor 保留 `errors.Is(context.Canceled)`，公共分类稳定为 499 `REQUEST_CANCELLED`；
- 当前 Attempt 和父 Request 以 `cancelled/client_cancelled` 终结，不被误记为 Transport、Protocol 或普通失败；
- 终态数据库写入使用 `context.WithoutCancel` 派生的 2 秒 bounded context，避免已取消的入站 Context 阻止审计落库，也不会产生无限后台任务。

自动化测试要求 Gateway 和真实 HTTP 上游均在取消后 1 秒内解除阻塞，并显式等待上游 Handler 收到取消，避免只验证客户端协程返回而遗漏连接泄漏。

## 6. 当前限制

- `stream=true` 明确返回 501 `CHAT_STREAMING_NOT_IMPLEMENTED`，由 P07 接通 SSE；不会把流式请求降级为普通响应。
- 当前每个请求只有一次 Attempt，所以 `gateway.attempt_count=1`；该 Attempt 已持久化，P08 引入安全重试后再从真实尝试数投影。
- Request/Attempt 生命周期已经持久化并由数据库约束；P06-T08 将补齐完整非流式 HTTP 端到端场景矩阵。
