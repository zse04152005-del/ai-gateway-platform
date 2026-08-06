# Usage Ledger Schema

> 状态：Implemented
>
> 日期：2026-08-03
>
> 对应任务：P10-T01

## 1. 事实边界

Migration 7 已把一次客户端调用建模为 `gateway_requests`，把每次真实上游调用建模为独立 `route_attempts`。Migration 13 在这两个事实根之上增加 `usage_ledger_entries`，一行表达一个 Token 类型、一个整数数量和一个计量来源。它不保存 Prompt、Response、Credential、Secret 或 Provider 原始 Payload。

`attempt_id` 可以为空：Provider 调用产生的用量归属具体 Attempt；缓存命中等没有物理调用但可归属于 Request 的事实仍能入账。所有行都必须通过 `(tenant_id, request_id)` 复合外键证明租户归属；非空 Attempt 还必须通过 `(request_id, attempt_id)` 证明它确实属于该 Request。

## 2. 幂等与不可变性

- `event_id` 使用 UUID，并由全局唯一约束保证同一事件最多形成一条有效分录；Migration 17 的不可变 Receipt 进一步保存规范化 Payload SHA-256，在至少一次投递下区分合法重放与“同 ID 不同事实”冲突。
- 表只允许 `INSERT`。数据库触发器对 `UPDATE` 和 `DELETE` 返回 SQLSTATE `23514`；Migration 20 的 Adjustment 同样只能追加，原始行和历史修正都不能覆盖。
- 普通 `quantity` 使用精确 `bigint`，范围为 `1..2^53-1`；Adjustment 的 quantity/amount 是有符号 delta，但每次写入后保存的绝对结果必须保持在 `0..2^53-1`。这样既能完全反向归零，也不会产生负用量或负账单。
- Migration 13 先让 `token_type` 与 `source` 接受最长 64 字符的小写安全标识符；Migration 14 再由 P10-T02 收紧为 `internal/metering` 定义的有限领域枚举，未知历史值会阻止迁移。

## 3. 查询与扩展边界

Tenant/Request/时间索引用于请求账单时间线，Request/Attempt/时间部分索引用于物理调用明细。`created_at` 表示账本落库时间，`observed_at` 保留事实观察时间；异步消费延迟不会覆盖原始时间。

Migration 15 追加 `price_version_id` 与 `amount_micros`：每条 Usage 必须通过 `(price_version_id, token_type)` 复合外键锁定一条已发布且在 `observed_at` 生效的费率，币种、区域和单位由不可变 PriceVersion 导出。P10-T05 Consumer 同时要求事件 `billing_unit` 与费率单位完全匹配，并以整数向上取整保存金额。P10-T06 已按 [`multi-attempt-cost-aggregation.md`](multi-attempt-cost-aggregation.md) 从不可变分录即时重建全部 Attempt 金额；P10-T08 的 signed Adjustment 进入同一即时聚合，单行负值有效，但最终 Attempt/Request 币种总额仍必须非负。后续字段只能通过追加迁移扩展，不能修改已应用的 Migration 13。

Migration 19 追加 `event_schema_version` 与 tokenizer/model 证据列。Schema v1 历史行保持证据为空；Schema v2 的 `source=estimated` 行必须完整保存 tokenizer/version、physical model、Deployment version 与 provider protocol version，其他来源必须为空。Consumer 把 Event 的同一证据原样写入 Ledger，不能用当前目录值覆盖历史估算身份。完整边界见 [`local-token-estimation.md`](local-token-estimation.md)。

Migration 20 让 Adjustment 通过 `adjusts_event_id` 引用一条原始 Provider/Estimated/Reconciled 分录，禁止 Adjustment 引用 Adjustment。`internal/meteringadjustment` 使用可信 Tenant/Project Scope 和原始行锁串行化并发写入，以绝对修正后数量/金额计算本次 delta；同一幂等键只生成一行，不同事实复用键或 Event ID 会冲突。数据库再次核对 Tenant、Request、Attempt、Token 类型和 PriceVersion 完全相同，并保存有限 `origin`、原因码、内容无关的 ticket/batch/incident reference 与操作者身份。Adjustment 不是 UsageEvent，因此 `event_schema_version` 和 tokenizer/model 字段必须为空。

## 4. 验证

真实 PostgreSQL 集成测试覆盖 Request 级与 Attempt 级分录、`event_id` 跨 Request 全局重复、Tenant/Request 和 Request/Attempt 错配、数量边界、有限分类、价格锁定、十次事件重放、Fingerprint 冲突、v1/v2 兼容、v2 估算证据完整性、Provider 禁带估算证据、16 路 Adjustment 并发幂等、跨 Scope/修正链/缺失审计字段拒绝、signed 成本聚合、数据库级 UPDATE/DELETE 拒绝，以及敏感内容列缺失。CI 在临时数据库执行最新迁移的受控 Down/Up 恢复。
