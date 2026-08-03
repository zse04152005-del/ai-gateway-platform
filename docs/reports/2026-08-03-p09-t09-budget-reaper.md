# P09-T09 过期预留 Reaper 验收报告

- 日期：2026-08-03
- 范围：数据库时间过期、批量领取、多实例并发、Account CAS、closed Account 回收、expire 审计事件与结算竞争
- 结论：实现与本地完整门禁通过；实现提交和 GitHub Actions 证据待补充

## 1. 实现结果

新增 `budget.PostgresReaper`。每次 `Reap` 按数据库 `clock_timestamp()` 选择最早到期的 pending Reservation，以 `FOR UPDATE SKIP LOCKED` 让多实例分片，再用 Account version CAS 在独立短事务内扣除完整 reserved、写入 `expired/actual=0/released=reserved/overage=0` 终态并追加唯一 expire Ledger。

默认 batch=100，最大 1000；CAS 默认 128 次、绝对最多 256 次，等待受 Context 控制。批内每个事件独立提交，后续失败会同时返回已提交的 partial Events 和错误。EventID 固定为 `expire:<reservation_uuid>` 并与 Ledger 幂等键一致，Ledger ID、整数金额、resulting balances 和数据库时间组成无内容审计/对账事实。

Settler 读取 Reservation 时增加 `FOR UPDATE`，与 Reaper 统一为 Reservation→Account 锁顺序。同一 Reservation 的结算/过期竞争只有一个终态：settle 先提交则 Reaper 不再选中，expire 先提交则迟到结算安全冲突。Migration 12 允许 closed Account 释放在途 hold，但不能重开或修改身份、周期、限额和 closure。

## 2. 并发与故障覆盖

- 12 个 open Account 过期 hold、4 个 closed Account 过期 hold和 1 个未来 hold；
- 首批 batch=3 精确返回 3 个事件并设置 AtCapacity；
- 8 个并发 Worker 分片回收，最终恰好 16 个唯一 EventID 和 16 个唯一 expire Ledger ID，无重复终态；
- open Account 最终只保留未来 hold 的 reserved=20，version 从 1 推进到 13；
- closed Account reserved 从 20 归零，version 从 1 推进到 5，状态仍为 closed；
- 重复扫描返回空结果，未来 Reservation 仍为 pending；
- Settler/Reaper 同时竞争到期 Reservation，最终只存在 reserve + settle 或 reserve + expire 两条 Ledger，Account reserved 必为 0；
- 非法 batch/retry、nil 依赖/Context、非法 Actor 和不一致审计事实均在单元测试中 fail closed。

## 3. 门禁结果

- `go test -count=20 ./internal/budget`：连续 20 轮通过；
- 真实 PostgreSQL `TestPostgresBudgetExpiredReservationReaper`：连续 20 轮通过，共验证 320 个过期 Reservation 事件及 20 次结算/过期竞争；
- 单元 profile 与 P09-T07/T08/T09 三个真实 PostgreSQL 预算场景 profile 按代码块合并：预算包覆盖率 84.1%（435/517 statements）；
- 常规与 integration build tag 的 golangci-lint：0 issue；
- `scripts/dev.ps1 -Action test-integration`：完整 PostgreSQL/Redis/进程集成套件通过，P09-T07/T08 回归同时通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、12 个迁移顺序、Actions 语法和本地密钥扫描全部通过。

## 4. 远端证据

实现提交和 GitHub Actions `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 结果将在推送后补充。
