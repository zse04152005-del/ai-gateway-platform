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

