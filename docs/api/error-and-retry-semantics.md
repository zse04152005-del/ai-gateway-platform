# 错误、超时、取消与重试语义

> 状态：Accepted for MVP  
> 日期：2026-07-30  
> 对应任务：P01-T05

## 1. 统一错误响应

```json
{
  "error": {
    "code": "MODEL_CAPACITY_EXHAUSTED",
    "message": "No healthy deployment is currently available",
    "type": "gateway_error",
    "param": null,
    "request_id": "req_...",
    "retryable": true,
    "retry_after_ms": 1000
  }
}
```

生产响应不包含堆栈、内部地址、Provider Key、原始供应商 Body 或其他租户信息。

### 1.1 实现边界

- `internal/apierror.Definition` 只允许保存经过格式校验的公开状态码、稳定错误码、固定消息、类型、参数路径和重试提示。
- `internal/apierror.Error` 私有保存内部 cause，并支持 `errors.Is`/`errors.As`；HTTP Renderer 绝不读取或序列化 `Error()`、cause 或堆栈。
- 未识别的普通 `error` 一律映射为 `500 INTERNAL_ERROR` 和固定消息；服务只能选择经过校验的公开 `type`。
- `request_id` 在 HTTP 传输边界注入，不写入可跨请求复用的领域错误。
- `Retry-After` HTTP Header 向上取整到秒，响应体保留毫秒值；只有 `retryable=true` 才允许设置。
- 公开 `param` 只能是字段路径字符，不允许携带用户内容、URL 或内部路径。

健康接口、未知路由和方法错误也使用同一 ErrorEnvelope，不返回 Go/`net/http` 默认纯文本错误页。

## 2. 超时层次

| 超时 | 含义 | 默认行为 |
|---|---|---|
| client deadline | 客户端声明的总时限 | 不超过平台最大值，传播上游 |
| connect timeout | TCP/TLS 连接 | 首包前可重试 |
| response header timeout | 等待响应头 | 首包前可重试 |
| first token timeout | 流式等待首模型事件 | 首包前可切换备用 |
| no-progress timeout | 流中长时间无事件 | 取消上游，返回部分失败 |
| write timeout | 客户端读取过慢 | 取消上游，标记 client_slow/cancelled |
| total gateway cap | 平台最大总时限 | 覆盖所有 Attempt，防重试放大 |

## 3. 重试判定

### 可重试候选

- 连接失败。
- 首包前超时。
- 429 且策略允许、Retry-After 在总预算内。
- 部分明确临时 5xx。
- 熔断探测失败后的其他健康 Deployment。

### 不可重试

- Key/权限错误。
- 参数、上下文长度、内容策略错误。
- 已向客户端发送模型内容。
- 工具调用可能已产生副作用且没有幂等保证。
- 总 Attempt、总时间或预算不足。

### 3.1 熔断归因与并发探测

P08-T05 已实现 Deployment 级 `Closed/Open/Half-Open`。只有可归因于 Provider 可用性的 429、容量、Timeout、5xx、协议和 Transport 故障进入失败阈值；Caller Cancellation、认证/权限、参数/上下文、内容策略和本地 Adapter 配置不触发熔断。

Open 到期只表示可以尝试恢复，不表示 Provider 已恢复。Selector 的健康读取不占用名额；选中后在真实 Attempt 前原子获取 Half-Open Permit，默认最多 2 个并发。Permit 带状态 Generation 且只能完成一次，旧 Generation 的迟到成功不能关闭刚刚重新 Open 的 Circuit。Open、Half-Open 饱和或状态容量不足对客户端统一映射为可重试 503 `MODEL_UNAVAILABLE`。

### 3.2 有限重试分类与预算

P08-T06 使用 `retry-classifier/v1` 把失败判定为 `no_retry`、`retry_allowed` 或 `different_deployment_only`。判定只读取稳定类型和已验证的 `NormalizedError`，不解析 Provider Body 或错误字符串。认证、权限、参数、上下文、内容策略、Caller Cancellation/Deadline、未知错误和本地 Adapter 配置不重试；429、Capacity、Timeout 和临时 5xx 必须带可信 retryable 事实。

所有可重试候选继续受最大 Attempt、额外费用许可、请求总 Deadline、下一 Attempt 最小窗口和 `Retry-After` 约束。已输出模型内容永不重试。Timeout、临时 5xx 和 Transport 在上游提交已发生或无法确认时只能更换 Deployment；Protocol 与首 Token 超时也只能更换 Deployment。完整矩阵和精确时间边界见 [`../architecture/retry-classification.md`](../architecture/retry-classification.md)。

### 3.3 有界故障切换编排

P08-T07 在非流式生产链路接入请求级顺序编排，默认最多 3 个物理 Attempt、总时限 30 秒。每次失败 Attempt 必须先持久化为独立 retryable_failed，再重选 Deployment 和创建新 Attempt；不存在隐藏的 Adapter 内重试。`different_deployment_only` 排除全部已尝试目标，普通 retry 优先备用目标、无替代时才允许原目标。固定策略在“只能换目标”的故障下停止，不能被容灾逻辑暗中绕过。

无备用时对外保留最后一个真实 Provider 错误；重选依赖失败才返回 `ROUTING_UNAVAILABLE`。公共 `gateway.attempt_count` 是真实物理调用数。每个失败 Attempt 保留已知 Usage，供 P10 按全部 Attempt 聚合费用。完整执行与事务边界见 [`../architecture/failover-orchestration.md`](../architecture/failover-orchestration.md)。

## 4. 首包定义

首包不是 HTTP 响应头，也不是 Gateway heartbeat。只有第一个客户端可见的模型内容、推理内容或工具调用 delta 才视为模型首包。

在首模型包之前可以选择备用 Deployment；之后不得在同一响应中拼接另一模型输出。

## 5. 部分失败

如果客户端已收到模型内容后发生 Provider 断流、解析失败、no-progress timeout 或平台取消：

- Request 状态为 `PARTIAL_FAILED`。
- Attempt 状态为 `PARTIAL_FAILED`。
- 保存已知 Token/字节/时间和供应商 usage；缺失部分可估算并明确来源。
- 预算按所有 Attempt 实际/估算费用结算。
- 流以结构化错误事件或连接终止结束，具体兼容行为在 OpenAPI/SSE 文档固定。

## 6. 客户端取消

- HTTP Context 取消后立即停止读取/写入并取消上游请求。
- Attempt 标记 `CANCELLED_BY_CLIENT`，但仍可生成已发生的 Usage。
- 记录取消传播耗时，不把 Provider 是否真正停止计费当作已保证事实。

## 7. 重试预算

每个请求同时受以下约束：

- 最大 Attempt 数。
- 最大总耗时。
- 最大预计额外费用。
- 各 Deployment 熔断/健康状态。
- 客户端 deadline。

任何一个约束耗尽都停止重试。

## 8. HTTP 状态建议

| 状态 | 场景 |
|---:|---|
| 400 | 参数或不支持能力 |
| 401 | 虚拟 Key 无效 |
| 403 | 模型/项目权限或策略阻断 |
| 408/504 | 请求或上游超时，按兼容策略选择 |
| 409 | 配置版本/幂等冲突 |
| 413 | 请求过大 |
| 429 | RPM/TPM/并发/预算或 Provider 容量 |
| 502 | Provider 协议或上游失败 |
| 503 | 无健康 Deployment/网关暂时不可用 |
| 500 | 未分类网关内部错误 |
