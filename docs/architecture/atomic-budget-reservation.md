# PostgreSQL 原子预算预留

P09-T07 在 P09-T06 的账户、预留和追加式账本之上实现权威费用 admission。一次成功预留必须同时推进 Account 余额、创建 Reservation、追加 reserve Ledger；任何引用、约束或提交失败都整体回滚。

## 1. 输入与幂等边界

`PostgresReserver.Reserve` 接收可信 `TenantID`、`AccountID`、Gateway `RequestID`、账户内 `IdempotencyKey`、正整数 `AmountMicros`、过期时间和安全 Actor。金额上界仍为 `2^53-1`，过期时间会归一到 PostgreSQL microsecond 精度。

每次尝试先按 `(tenant_id, account_id, idempotency_key)` 查询既有 Reservation：

- Request、金额和 ExpiresAt 全部一致时返回原 Reservation 与原 reserve Ledger，`Idempotent=true`；即使账户后来已满，也不会重复占用；
- 同一个 Key 的任一事实不同都返回 `ErrIdempotencyConflict`；
- 查询必须包含 Tenant 和 Account，跨租户调用只得到 `ErrAccountNotFound`，不能探测其他租户事实；
- 已有 Reservation 缺少对应 reserve Ledger 视为存储损坏并 fail closed。

## 2. CAS 事务

不存在幂等事实时，每次短事务执行：

1. 读取并校验 Account：`open`、位于半开周期、余额为精确整数；
2. 在应用层先判断 `committed + reserved + requested <= hard`；
3. 执行带 `tenant_id + id + version` 的条件 UPDATE，并在 SQL 中再次判断状态、周期和 hard；
4. UPDATE 成功后插入 pending Reservation；
5. 追加 `entry_kind=reserve` 的 Ledger，记录 `reserved_delta` 和 resulting balances；
6. 最后提交事务。

UPDATE 由 PostgreSQL 行锁串行化竞争者，条件在取得锁后重新检查。版本不匹配或序列化/死锁冲突只触发下一次完整事务；账户更新绝不会脱离 Reservation/Ledger 单独提交。Request 外键失败、UUID 冲突、CHECK 失败或连接错误返回稳定安全错误，底层 DSN/SQL 细节不进入错误字符串。

## 3. hard、soft 与时间

hard 是 admission 条件，超过时返回 `ErrBudgetExceeded`，不增加 version，也不创建任何事实。soft 不拒绝；成功结果在 resulting committed+reserved 严格大于 soft 时设置 `SoftLimitExceeded`，等于 soft 仍未超过。

余额更新时间不使用协程预先取得的主机时间。UPDATE 在真正取得行锁后写入 `GREATEST(updated_at, clock_timestamp())`，Reservation created/updated 和 Ledger occurred 复用 RETURNING 的同一个数据库权威时间，从而在高并发、调度延迟或细微主机时钟偏差下保持单调。

## 4. 有界冲突重试

默认最多 128 次 CAS，硬上限 256；每次冲突之间执行可取消的固定短延迟。Context 取消会停止等待，绝不在后台继续占用。`MaxAttempts=1` 的受控行锁测试证明第一次版本冲突立即返回 `ErrRetryExhausted`；`MaxAttempts=2` 在相同冲突后第二次成功且结果报告 `Attempts=2`。

上限是进程资源保护，不是绕过 hard 的理由。重试耗尽、数据库不可用或存储事实不完整都 fail closed，调用方不能把它们当作预算允许。

## 5. 并发验收

真实 PostgreSQL 测试为同一 hard=100 的 Account 建立 160 个不同 Gateway Request，并同时预留 1 micro：必须恰好 100 个提交、60 个 `ErrBudgetExceeded`，最终 Account 为 `reserved=100/version=101`，Reservation 和 reserve Ledger 各 100 条，resulting reserved 覆盖且只覆盖 `1..100`。测试同时验证 soft=80 只对 81..100 标记、hard 已满后的幂等重放、冲突 Key、跨租户隐藏和 Request FK 失败整事务回滚。
