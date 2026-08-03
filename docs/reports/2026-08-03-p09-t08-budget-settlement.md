# P09-T08 预算结算与差额释放验收报告

- 日期：2026-08-03
- 范围：多 Attempt、缓存命中、失败/取消费用、差额释放、超额保留、关闭账户结算与终态幂等
- 结论：实现与本地完整门禁通过；实现提交和 GitHub Actions 证据待推送后补充

## 1. 实现结果

新增 `budget.PostgresSettler` 和严格的 `SettlementInput` Outcome/Charge 矩阵。成功必须包含 Attempt，失败与取消允许零个或多个 Attempt，缓存命中必须且只能包含一个 Cache；重复引用、混合类型、非法引用和 `2^53-1` 求和溢出均在进入数据库前拒绝。

结算以 Tenant-qualified Reservation 为根，在同一短事务内验证 Gateway Request 终态、以 Account version CAS 扣除完整预留并增加 actual、终结 Reservation、记录 released/overage，并追加唯一 `settle` 或 `release` Ledger。actual 小于预留时释放差额，大于预留或 hard 时仍保存真实费用；只有精确整数总上界、余额不一致或数据库事实损坏会 fail closed。零费用取消使用 release，其余成功、失败、缓存和部分取消使用 settle。

Migration `000012_allow_closed_budget_settlement` 允许 closed Account 继续以 `version+1` 推进余额，使关闭周期后仍在途的 Request 可以结算；账户不能重开，closure、身份、周期和限额仍不可修改。终态重放只有在 Request 状态、Reservation 状态、actual、Ledger kind 和幂等键一致时返回原事实，冲突命令不重复记账。

## 2. 并发与场景覆盖

- closed Account 的两个 Attempt 成功结算：预留 100、actual 70、released 30，账户保持 closed；
- 失败结算：预留 80、actual 120、hard 100，committed 保存 120、overage 40，剩余 hard 为 0；
- 缓存命中：预留 100、一个 Cache Charge actual 5、released 95，重放返回同一 Reservation/Ledger；
- 零费用取消：actual 0、released 100，Reservation 为 cancelled，Ledger 为 release；
- 部分费用取消：actual 20、released 80，Reservation 为 cancelled，Ledger 为 settle；
- 同一成功命令 64 路并发只有一个 `Idempotent=false`，其余均读取相同 Reservation 和终态 Ledger；每个账户最终只有 reserve + terminal 两条 Ledger；
- 冲突 actual、未知 Reservation 和 Gateway Request Outcome 不匹配均拒绝且不改写终态。

专项重复运行实际发现并修复两类缺陷：第一轮暴露 PostgreSQL 对 CAS 表达式 `$7 - $3` 的参数类型歧义，金额参数现显式转换为 `bigint`；20 轮压力交错暴露“已读 pending、随后读到另一事务结算后的 Account”被误判为普通冲突，该状态现进入有界重试并在新事务读取原终态事实。

## 3. 本地门禁结果

- `go test -count=20 ./internal/budget`：连续 20 轮通过；
- 真实 PostgreSQL `TestPostgresBudgetSettlementAndDifferenceRelease`：连续 20 轮通过，共执行 1,280 次同 Reservation 并发结算调用；
- 单元 profile 与真实 PostgreSQL settlement integration profile 按相同代码块合并：预算包覆盖率 81.0%（353/436 statements）；
- 常规与 integration build tag 的 golangci-lint：0 issue；
- Migration 校验：12 个文件对完整，`12 → 11 → 12` 后均为 `dirty=false`；
- `scripts/dev.ps1 -Action test-integration`：完整 PostgreSQL/Redis/进程集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、迁移顺序、Actions 语法和本地密钥扫描全部通过。

## 4. 远端证据

实现提交、GitHub Actions `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 结果，以及清单证据提交将在远端门禁完成后补充。
