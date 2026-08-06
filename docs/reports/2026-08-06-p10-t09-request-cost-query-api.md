# P10-T09 Request Cost 查询 API 验收报告

- 日期：2026-08-06
- 范围：Tenant/Project Scope、费用查询 API、不可变分录与价格明细、安全错误映射
- 结论：实现、本机完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

实现提交 `1ebbe70` 在控制面增加：

```text
GET /admin/v1/tenants/{tenantId}/projects/{projectId}/requests/{requestId}/cost
```

端点调用 `internal/meteringcost.PostgresAggregator`，在 PostgreSQL repeatable-read 只读快照中从不可变 Usage Ledger 即时重建结果。响应按 Attempt 序号保留所有物理调用和独立 Request-level bucket，并为每条 Ledger 分录返回 Event/Attempt、Token 类型、数量、来源、时间、冻结的 PriceVersion/Rate、币种、计费单位与整数 micros 金额。Request 与各 bucket 的 totals 始终按币种分离并排序。

P10-T08 signed Adjustment 不覆盖原始事实。查询同时返回修正目标 Event、有限 Origin、原因码、安全审计 Reference、Actor、修正后的绝对数量与金额，因此调用方可以从原始分录、signed delta 和最终结果解释费用变化。

## 2. Scope 与安全边界

费用路径显式携带 Tenant、Project 与 Request。Aggregator 必须在同一查询中匹配完整 Scope；不存在、跨租户和跨项目统一映射为 `404 REQUEST_COST_NOT_FOUND`，不泄露其他租户 Request 是否存在。生产部署由控制面入口外层的 Admin OIDC 完成认证和 Tenant/Project 授权，OpenAPI 已声明该安全边界。

数据库读取只涉及执行身份、计量分类、冻结价格和 Adjustment 审计元数据。响应不读取或返回 Prompt、Response Body、Virtual/Provider Credential、Provider 原始 Usage 或外部 Raw Evidence，并使用 `Cache-Control: no-store` 与 `X-Content-Type-Options: nosniff`。

## 3. 完整性与错误语义

- 非法 Scope 或 Request ID：`400 INVALID_COST_QUERY`；
- 不存在或跨 Scope：`404 REQUEST_COST_NOT_FOUND`；
- Request/Attempt 尚未终态：`409 REQUEST_NOT_TERMINAL`；
- 终态 Outbox 尚未全部形成 Ledger：`409 REQUEST_COST_PENDING` 与 `Retry-After: 1`；
- 数据库读取失败、事实漂移、非法枚举、负最终总额或精确整数溢出：`503 REQUEST_COST_UNAVAILABLE`。

错误响应不包含底层连接串或存储错误。异步 pending 不会降级为零费用；retryable-failed、failed、partial-failed、cancelled 与 succeeded Attempt 均不会因状态被过滤。

## 4. 本机门禁

- `internal/meteringcost`、`internal/controlplane` 与 `cmd/control-plane` 单元测试通过；
- 真实 PostgreSQL API 专项连续 20 轮通过，覆盖 active、pending、跨 Scope，以及 retryable-failed 2,500,000 micros 加最终成功 5,000,000 micros 的两 Attempt 明细与 7,500,000 micros 总额；
- 完整 PostgreSQL、Redis、Redpanda integration 通过；
- `scripts/dev.ps1 -Action check` 通过依赖校验、格式、双标签 lint、全量单测、构建、漏洞扫描、20 个 Migration 顺序、Actions 配置和敏感信息扫描；
- OpenAPI YAML 解析成功，远端 YAML 门禁通过；本任务没有 Schema 变更，数据库保持 `version=20 dirty=false`。

## 5. 远端证据

GitHub Actions [`31066986306`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/31066986306) 三个 Job 全绿：`go-quality` 通过 Linux race、进程生命周期、lint、构建与漏洞门禁；`migration-integration` 通过完整 Migration 生命周期与真实 PostgreSQL/Redis/Redpanda 集成；`config-and-secrets` 通过 OpenAPI YAML 和双重密钥扫描。
