# PostgreSQL 预算结算与差额释放

P09-T08 在原子预留之后实现终态费用核对。结算只接收已完成定价的无内容 Charge 事实，把整份 Reservation 从 reserved 扣除、把实际费用计入 committed，并在同一 PostgreSQL 事务中终结 Reservation、追加 Ledger。调用失败时三个事实源必须一起回滚。

## 1. Outcome 与 Charge 规则

`SettlementInput` 包含可信 Tenant、Account、Reservation、Gateway Request、Outcome、Charge 列表和审计 Actor。Charge 只保存类型、稳定引用和整数 micros，不携带提示词、响应或 Provider 凭据。所有 Charge 求和前检查重复引用和 `2^53-1` 上界。

| Outcome | 合法 Charge | Request 终态 | Reservation 终态 | Ledger |
| --- | --- | --- | --- | --- |
| `succeeded` | 一个或多个 Attempt | `succeeded` | `settled` | `settle` |
| `failed` | 零个或多个 Attempt | `failed` / `partial_failed` | `settled` | `settle` |
| `cache_hit` | 恰好一个 Cache | `succeeded` | `settled` | `settle` |
| `cancelled` | 零个或多个 Attempt | `cancelled` | `cancelled` | actual=0 时 `release`，否则 `settle` |

失败和取消不能假定费用为零：Provider 已执行的 Attempt 仍必须计入 actual。缓存命中不能混入 Attempt，成功也不能在没有 Attempt 的情况下伪造费用。相同类型和引用的 Charge 重复出现会被拒绝，防止一个物理事实重复计费。

## 2. 差额与超额

结算始终释放完整预留，再按 actual 增加 committed：

```text
released = max(reserved - actual, 0)
overage  = max(actual - reserved, 0)
new_reserved  = account.reserved - reservation.reserved
new_committed = account.committed + actual
```

actual 小于预留时，差额只体现在 Reservation 的 released 和账户 resulting balance，不额外写第二条 release Ledger；一次终态只追加一条权威 Ledger。actual 大于预留或 hard 时仍保存真实费用和 overage，后续 admission 会因 committed 已超 hard 而停止。只有超过精确整数总上界或账户缺少对应 reserved 才 fail closed。

## 3. CAS 事务与关闭账户

每次尝试使用短事务：

1. 按 Tenant、Account、Reservation 读取 pending 或既有终态事实；
2. 验证 Reservation 的 Request 和 Gateway Request 终态与 Outcome 一致；
3. 读取 Account，并用 `tenant_id + id + version` 条件 UPDATE 原子扣减 reserved、增加 committed、推进 version；
4. 以相同数据库时间终结 Reservation，记录 actual、released、overage；
5. 追加 `settle` 或 `release` Ledger，记录 signed delta 和 resulting balances；
6. Commit 后才返回成功。

版本冲突、序列化失败和死锁按与预留相同的可取消、有上限策略重新开始完整事务。Migration `000012_allow_closed_budget_settlement` 只放宽 closed Account 的余额推进：账户不能重开，closure、身份、周期和限额仍不可修改。这样周期关闭后已经 admission 的在途请求仍可结算，不会吞掉真实费用。

## 4. 幂等与冲突

竞争者观察到 Reservation 已为终态时，不再更新任何余额，而是读取原 Account、Reservation 和终态 Ledger。只有 Request 状态、预期 Reservation 状态、actual、Ledger kind 和稳定幂等键全部一致才返回 `Idempotent=true`；费用或 Outcome 冲突返回 `ErrSettlementConflict`。未知 Tenant/Account/Reservation 组合统一表现为 Reservation not found，不能用跨租户 ID 探测事实。

底层 SQL、DSN 和约束细节不进入公开错误字符串。终态 Reservation 缺少 Ledger、余额非法或持久化事实不能通过领域校验时返回 unavailable 并 fail closed，调用方不得自行补写部分事实。

## 5. 验收边界

真实 PostgreSQL 集成测试覆盖关闭账户的两 Attempt 成功、失败费用超过预留/hard、缓存差额释放、零费用取消、部分费用取消、冲突重放、未知 Reservation 和 Request Outcome 不匹配。相同成功命令 64 路并发必须只有一个非幂等提交，其余全部读取相同 Reservation/Ledger；每个账户最终只能有一条 reserve 和一条终态 Ledger。
