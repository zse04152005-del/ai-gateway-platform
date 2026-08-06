# Request Cost 查询 API

P10-T09 在控制面提供以下只读端点：

```text
GET /admin/v1/tenants/{tenantId}/projects/{projectId}/requests/{requestId}/cost
```

它面向经外层 Admin OIDC 授权的运营、财务与审计调用方。Tenant 与 Project 都是查询条件，而不是客户端可覆盖的身份声明；Request 必须同时属于这两个 Scope。不存在和跨 Scope 查询统一返回 `404 REQUEST_COST_NOT_FOUND`，避免通过 Request ID 枚举其他租户的存在性。

## 完整性边界

查询在 PostgreSQL repeatable-read 只读事务中即时重建，不维护第二份可漂移的费用快照。返回结果前必须同时满足：

1. GatewayRequest 已进入 `succeeded`、`partial_failed`、`failed` 或 `cancelled` 终态；
2. 该 Request 的全部 RouteAttempt 均已终态；
3. 每条可计费 Usage Event Outbox 事实都已有对应 Usage Ledger 分录；
4. Ledger、PriceVersion、PriceRate 与 Adjustment 元数据满足有限枚举、精确整数和引用约束；
5. 每个 Attempt 及整个 Request 的每币种最终金额均非负且不超过 `2^53-1`。

Request 仍活动时返回 `409 REQUEST_NOT_TERMINAL`。终态已形成但异步 Metering Consumer 尚未把全部 Outbox 事件落账时，返回 `409 REQUEST_COST_PENDING` 与 `Retry-After: 1`；不会把尚未到账误报为零费用。存储事实损坏、读取失败或无法形成安全投影时统一返回 `503 REQUEST_COST_UNAVAILABLE`，且不回显底层数据库错误。

## 响应结构

`attempts` 按 Attempt 序号返回所有真实物理调用，包括 retryable failed、failed、partial failed、cancelled、最终 succeeded 以及没有用量的零费用 Attempt。`request_level` 保留无 Attempt 归属的请求级事实。每个 bucket 都包含：

- `ledger_entry_count` 与完整 `entries`；
- 每条分录的 Event/Attempt、Token 类型、数量、来源和时间；
- 冻结的 `price_version_id`、币种、计费单位、单位数量和单位价格；
- 使用整数 micros 表示的分录金额；
- 按币种分离并排序的 `totals`，不同币种不得相加。

来源为 `adjustment` 的 signed 分录还包含目标 Event、受限 Origin/Reason、安全审计 Reference、Actor，以及修正后的绝对数量和金额。原始分录和修正分录都保留在响应中，调用方无需通过覆盖历史事实来解释最终合计。

## 数据最小化与部署要求

数据库查询只读取执行标识、计量分类、冻结价格和 Adjustment 审计元数据。API 不读取也不返回 Prompt、模型响应正文、Virtual/Provider Credential、Provider 原始 Usage 或外部证据正文。

当前 Go Handler 承担 Scope 解析、稳定错误映射和安全响应；Admin OIDC 的令牌校验与细粒度 Tenant/Project 授权属于控制面入口前的部署信任边界。生产环境不得绕过该边界直接暴露端点。响应使用 `Cache-Control: no-store` 与 `X-Content-Type-Options: nosniff`，避免费用和审计元数据被共享缓存持久化或被错误解释。
