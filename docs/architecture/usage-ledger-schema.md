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

- `event_id` 使用 UUID，并由全局唯一约束保证同一事件最多形成一条有效分录；后续 Metering Consumer 可以用唯一冲突实现至少一次投递下的幂等。
- 表只允许 `INSERT`。数据库触发器对 `UPDATE` 和 `DELETE` 返回 SQLSTATE `23514`；修正必须在 P10-T08 通过新的 Adjustment 分录表达。
- `quantity` 使用精确 `bigint`，范围为 `1..2^53-1`，避免 JSON/JavaScript 控制面交换时失真，也拒绝没有事实价值的零数量分录。
- Migration 13 先让 `token_type` 与 `source` 接受最长 64 字符的小写安全标识符；Migration 14 再由 P10-T02 收紧为 `internal/metering` 定义的有限领域枚举，未知历史值会阻止迁移。

## 3. 查询与扩展边界

Tenant/Request/时间索引用于请求账单时间线，Request/Attempt/时间部分索引用于物理调用明细。`created_at` 表示账本落库时间，`observed_at` 保留事实观察时间；异步消费延迟不会覆盖原始时间。

本迁移不包含价格版本、币种、金额和 Adjustment 引用：价格锁定属于 P10-T03，多 Attempt 金额聚合属于 P10-T06，修正引用属于 P10-T08。后续字段只能通过追加迁移扩展，不能修改已应用的 Migration 13。

## 4. 验证

真实 PostgreSQL 集成测试覆盖 Request 级与 Attempt 级分录、`event_id` 跨 Request 全局重复、Tenant/Request 和 Request/Attempt 错配、数量边界、安全标识符、数据库级 UPDATE/DELETE 拒绝，以及敏感内容列缺失。CI 还执行 `13 -> 12 -> 13`，验证最新迁移可受控回滚和恢复。
