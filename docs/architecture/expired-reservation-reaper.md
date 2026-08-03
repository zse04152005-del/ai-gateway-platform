# PostgreSQL 过期预留 Reaper

P09-T09 回收进程异常退出或终态链路中断后遗留的 pending Reservation。Reaper 是可信的跨租户后台组件，以 PostgreSQL 时间和索引为权威，每次调用只处理有界批次；每个过期预留独立提交，释放结果通过 `expired` Reservation 与追加式 `expire` Ledger 持久化为审计/对账事件。

## 1. 过期边界与批次

候选条件固定为 `status='pending' AND expires_at <= clock_timestamp()`，不使用 Worker 主机时间，也不依赖调用方传入 Tenant。查询按 `(expires_at, id)` 选择最早事实，并使用 Migration 11 的 pending-expiry 部分索引。

默认每批最多 100 条，允许范围为 1～1000。`AtCapacity=true` 表示本次达到批次上限，调用方应尽快继续扫下一批；空结果表示当前没有可获取的过期行，但并发 Peer 可能仍持有候选锁。Context 取消会停止新的事务和冲突等待。

一次 `Reap` 内的每条事件是独立事务。若前几条已经提交、后续条目失败，返回值保留已提交 Events 并同时返回错误，调用方不能把整批误认为原子回滚。重新调用只会选择仍为 pending 的行，不会重复终结已完成事实。

## 2. 并发领取与锁顺序

候选 Reservation 使用 `FOR UPDATE SKIP LOCKED`：多个 Reaper 实例不会相互等待同一行，而是分片处理其他到期事实。Reaper 和 Settler 都先锁定 Reservation，再读取/更新 Account，统一锁顺序避免“Account→Reservation”和“Reservation→Account”交叉死锁。

锁定候选后仍以 Account `tenant_id + id + version` 做 CAS。不同 Reservation 可能属于同一 Account，因此版本冲突、序列化失败和死锁会回滚该条短事务，并按与原子预留相同的可取消上限重新领取；默认 128 次、绝对最多 256 次。Account reserved 小于该 Reservation 的 hold 说明存储事实损坏，直接 fail closed，不能把负余额当作成功释放。

## 3. 原子回收与审计事件

每条成功回收在同一事务中：

1. 从 Account `reserved` 扣除该 Reservation 的完整预留，committed 不变，version 加一；
2. 把 Reservation 从 pending 变为 expired，写入 `actual=0`、`released=reserved`、`overage=0` 和数据库权威 terminal time；
3. 追加一条 `entry_kind=expire` Ledger，`committed_delta=0`、`reserved_delta=-reserved`，保存 resulting balances；
4. Commit 后返回 `ExpirationEvent`。

`ExpirationEvent.EventID` 与 Ledger 幂等键同为 `expire:<reservation_uuid>`，Ledger identity ID 是可排序证据。事件只包含 Tenant/Account/Reservation/Request 引用、整数金额和时间，不包含提示词、响应、用户名或 Provider 凭据。即使调用方在 Commit 后断线，持久化 Ledger 仍是权威审计/对账来源；下游应从 Ledger 按 ID 增量读取并以 EventID 幂等消费。

## 4. 与结算和账户关闭竞争

`expires_at` 是释放 hold 的硬边界。Reaper 与 Settler 同时竞争同一 Reservation 时，行锁保证只能有一个终态：Settler 先提交则 Reaper 不再选中；Reaper 先提交则迟到的结算返回 `ErrSettlementConflict`，不能把 expired 覆盖为 settled。调用方应让预留 TTL 覆盖最长合法执行与结算延迟；出现 `expire` 事件意味着需要对账，而不是默认为请求没有产生 Provider 成本。

Reaper 不要求 Account 仍为 open。Migration 12 允许 closed Account 继续推进余额和 version，因此周期关闭不能造成永久 hold；账户身份、限额、closure 和 closed 状态仍不可变。尚未到期的 pending Reservation 完全不触碰。

## 5. 验收边界

真实 PostgreSQL 测试建立 16 个过期 hold、一个未来 hold，并让 8 个并发 Worker 以 batch=3 回收：必须得到 16 个唯一 EventID/Ledger ID，未来 hold 保留，open Account 只剩未来 reserved，closed Account reserved 归零。随后让 Settler 与 Reaper 同时竞争同一到期 Reservation，最终必须且只能存在 reserve + settle 或 reserve + expire 两条 Ledger。
