# P09-T06 预算账本与预留表验收报告

- 日期：2026-07-31
- 范围：五维预算账户、幂等预留、追加式账本、Migration 11
- 结论：实现、专项、Migration lifecycle 与本地完整门禁通过；GitHub Actions 证据待提交后回填

## 1. 实现结果

新增 `internal/budget` 领域模型和 Migration `000011_create_budget_ledger`。金额统一为整数 micros，上界固定为 `2^53-1`；soft/hard、committed/reserved、Reservation actual/released/overage 和 Ledger signed delta 均拒绝浮点或越界值。

`app.budget_accounts` 使用严格 Scope Shape 和五个部分唯一索引，让 Tenant、Project、VirtualKey、用户及会话在同一币种/周期内独立建账。Project 与 VirtualKey 使用 Tenant-qualified 复合外键；用户/会话只保存最长 128 字符的 tenant-local opaque ref。账户身份、周期和限额不可变，余额或 open→closed 更新必须使 version 单调加一。

`app.budget_reservations` 强引用同 Tenant 的 Account 与 Gateway Request，并以 `(account_id, idempotency_key)` 去重。状态只允许 pending→settled/cancelled/expired；终态必须完整满足 released/overage 差额恒等式。`app.budget_ledger_entries` 保存 signed delta 和 resulting balances，Reservation 引用必须与 Tenant/Account 一致，触发器拒绝 UPDATE/DELETE。

## 2. 自动化覆盖

- Go 模型覆盖五种 Scope 的合法形状及混合/缺失字段拒绝；
- Account 覆盖周期、币种、soft/hard、精确整数上界、余额和值域、版本、Actor 与关闭状态；
- Reservation 覆盖 pending 形状、三种终态、释放差额、超额和非法生命周期；
- Ledger 覆盖 reserve/settle/release/expire/adjustment 形状、作用域引用、signed delta 和 resulting balance 上界；
- 真实 PostgreSQL 创建五种账户并核对字段形状和周期唯一性；
- 真实 PostgreSQL 拒绝跨 Tenant Project/Key/Account 引用、非法金额和非法终态；
- Account/Reservation 触发器验证单向状态与 version+1，Ledger 验证追加不可变；
- Migration lifecycle 验证空库 up、重复 up/no-change、`11 → 10 → 11` 和 production down guard。

## 3. 验收矩阵

| 验收项 | 权威证据 | 当前状态 |
| --- | --- | --- |
| Tenant/Project/Key/User/Session 可独立建账 | `TestBudgetLedgerSchemaConstraints` 的五种真实记录和部分唯一索引断言 | 真实 PostgreSQL 20 轮通过 |
| 跨租户绑定不可创建 | Project、VirtualKey、Account、Request 的复合外键约束断言 | 真实 PostgreSQL 20 轮通过 |
| 金额可跨 PostgreSQL/Go/后续 Redis 无损交换 | `2^53-1` CHECK 与 Go `MaximumAmount` 边界单测 | Go 20 轮和数据库专项通过 |
| 预留与账户生命周期不可倒退 | 数据库触发器正反例及版本断言 | 真实 PostgreSQL 20 轮通过 |
| 账本不能被修改或删除 | append-only trigger 的 SQLSTATE 断言 | 真实 PostgreSQL 20 轮通过 |
| 回滚后可重新前滚 | Migration `11 → 10 → 11` 且 `dirty=false` | 通过 |

## 4. 门禁结果

- `go test -count=20 -cover ./internal/budget`：连续 20 轮通过，覆盖率 98.2%；
- `TestBudgetLedgerSchemaConstraints`：真实 PostgreSQL 连续 20 轮通过；
- Migration `11 → 10 → 11`：production down guard 明确拒绝；开发库 `11 dirty=false → 10 dirty=false → 11 dirty=false`，恢复后 Schema 专项再次通过；
- `scripts/dev.ps1 -Action test-integration`：真实 PostgreSQL/Redis 完整集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、迁移顺序、Actions 语法和本地密钥扫描全部通过；迁移校验为 `count=11 latest=000011_create_budget_ledger`；
- 实现提交及 GitHub Actions 三 Job：待提交后回填。
