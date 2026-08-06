# meteringcost

P10-T06 的请求级费用聚合。它在 PostgreSQL repeatable-read 只读快照中验证 Request 已终态、每个 RouteAttempt 已终态，并确认该 Request 的每条 Usage Event Outbox 事实都已形成对应 Usage Ledger，随后从不可变分录即时重建结果。

结果保留所有物理 Attempt，包括没有正数量 Usage 的零费用 Attempt；失败、retryable failed、partial failed、cancelled 与最终成功均不会被状态过滤。Request 级缓存等无 Attempt 事实进入独立 bucket。P10-T08 的 signed Adjustment 与原始分录一起求和，单行负值不会被过滤，但最终 Attempt/Request 币种总额不得为负。不同 PriceVersion 的币种分别汇总，禁止跨币种机械相加；任一最终币种总额超过 `2^53-1` 时 fail closed。

异步消费者尚未落完账时返回 `ErrPending`，不会把暂时缺失的分录显示为零费用。查询以可信 Tenant+Project Scope 约束 Request，不存在、跨租户和跨项目统一返回 `ErrNotFound`。

P10-T09 已通过控制面 `GET /admin/v1/tenants/{tenantId}/projects/{projectId}/requests/{requestId}/cost` 暴露完整投影。每条分录返回 Token 类型、数量、来源、时间、冻结的 PriceVersion/Rate、币种与整数 micros 金额；Adjustment 额外返回目标事件、来源、原因、安全引用、Actor 和修正后的绝对数量/金额。响应不读取或返回 Prompt、Response、Credential、Provider 原文或 Raw Evidence，控制面部署仍必须在路由外层实施 Admin OIDC 授权。
