# 预算账户、预留与账本

P09-T06 为后续预算 admission、结算和过期回收建立 PostgreSQL 事实源。预算可在 Tenant、Project、VirtualKey、用户和会话五种维度独立建账；每个账户只属于一个 Tenant、一个币种和一个半开周期 `[period_start, period_end)`。

## 1. 金额与作用域

所有金额统一保存为整数 `micros`，范围是 `0..2^53-1`。该上界使 PostgreSQL `bigint`、Go、JSON 数字和后续 Redis Lua 都能无损交换，不使用浮点数参与 admission 或结算。账户的 soft/hard 必须大于零且 `soft <= hard`；已结算金额允许超过 hard，以完整记录 Provider 实际超额，但 `committed + reserved` 仍不能溢出精确整数范围。

`app.budget_accounts.scope_kind` 决定唯一合法的 Scope 形状：

| Scope | 强引用/标识 | 周期内唯一键 |
| --- | --- | --- |
| Tenant | `tenant_id` | Tenant、币种、周期 |
| Project | `(tenant_id, project_id)` | Tenant、Project、币种、周期 |
| Key | `(tenant_id, project_id, virtual_key_id)` | Tenant、Project、Key、币种、周期 |
| User | Tenant-local `principal_ref` | Tenant、用户引用、币种、周期 |
| Session | Tenant-local `session_ref` | Tenant、会话引用、币种、周期 |

Project 和 VirtualKey 使用复合外键，不能跨 Tenant 或跨 Project 绑定。用户/会话引用是最长 128 字符的 opaque ref，调用方只应写入稳定摘要或不可逆内部标识，不能保存用户名、提示词或其他个人内容。五个部分唯一索引允许不同维度独立建账，同时阻止同一维度在同一周期重复账户。

## 2. 三类持久化事实

`app.budget_accounts` 保存当前余额快照：`committed_amount_micros` 是已结算费用，`reserved_amount_micros` 是在途占用，`version` 为后续 CAS 更新提供单调版本。账户身份、周期和初始限额创建后不可修改；只允许余额推进和 `open → closed`，每次更新必须使版本加一且更新时间单调。

`app.budget_reservations` 把一个账户和一个 Gateway Request 强关联。`(account_id, idempotency_key)` 唯一，保证同一账户的 admission 重试不会创建第二份占用。状态只能从 `pending` 单向进入 `settled`、`cancelled` 或 `expired`；终态必须同时记录 actual、released、overage 和 terminal time，并满足：

```text
released = max(reserved - actual, 0)
overage  = max(actual - reserved, 0)
```

`app.budget_ledger_entries` 是追加式余额变更证据。每条记录包含 signed committed/reserved delta 和变更后的两个余额；reserve、settle、release、expire 必须引用同 Tenant、同 Account 的 Reservation，adjustment 不引用 Reservation。`(account_id, idempotency_key)` 唯一，UPDATE/DELETE 由触发器拒绝。

## 3. 事务边界与后续写入规则

迁移只建立事实模型，不实现 P09-T07/P09-T08 的 admission 算法。后续写路径必须在同一个 PostgreSQL 事务中：锁定或 CAS 账户版本、校验 hard、写入/终结 Reservation、更新账户余额、追加 Ledger Entry。任何一步失败都必须整体回滚，不能只更新余额或只写账本。

预留阶段写入 `reserved_delta > 0`；结算阶段把整份预留从 reserved 扣除并把 actual 加到 committed；取消或过期只释放 reserved。Ledger 的 resulting balances 必须等于同事务结束时 Account 快照，重试使用稳定 idempotency key 读取既有事实。

## 4. 生命周期与回滚边界

Migration `000011_create_budget_ledger` 还为 `gateway_requests (tenant_id, id)` 增加唯一约束，使 Reservation 的 Request 外键同时验证 Tenant。Down 会先移除触发器和三张预算表，再移除该约束；它会丢失所有预算事实，只能用于明确批准的开发回滚。

数据库约束负责租户隔离、合法 Scope、金额精度、单向状态和追加性。原子 admission、多 Attempt 结算与过期 Reaper 已分别在 [`atomic-budget-reservation.md`](atomic-budget-reservation.md)、[`budget-settlement.md`](budget-settlement.md) 和 [`expired-reservation-reaper.md`](expired-reservation-reaper.md) 实现；数据库角色权限与告警仍由后续任务补齐，所有写路径都不能以直接 SQL 绕过这些边界。
