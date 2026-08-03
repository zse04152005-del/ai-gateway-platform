# P09-T07 原子预算预留验收报告

- 日期：2026-08-03
- 范围：PostgreSQL Account version CAS、hard admission、幂等 Reservation 与 reserve Ledger
- 结论：实现、本地完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

新增 `budget.PostgresReserver`。每次 Reserve 先在可信 Tenant/Account 内查询幂等事实，再用短事务读取 Account、双重校验 hard、按 version 条件更新 reserved/version、插入 pending Reservation 并追加 reserve Ledger；只有最后 Commit 成功才返回允许。

默认 CAS 上限 128、绝对上限 256，冲突等待受 Context 控制。数据库权威 `clock_timestamp()` 在取得行锁后推进单调 updated_at，Reservation 与 Ledger 复用同一返回时间。稳定错误区分 hard 超限、账户不存在/非活动、幂等冲突、重试耗尽、引用冲突和基础设施不可用，私有数据库错误不进入公开字符串。

## 2. 并发与故障覆盖

- 160 个不同 Gateway Request 同时对 hard=100 的账户各预留 1 micro；
- 恰好 100 个成功、60 个 `ErrBudgetExceeded`，无重试耗尽或其他错误；
- Account 最终 `reserved=100/version=101`，Reservation/Ledger 各 100 条；
- resulting reserved 唯一覆盖 `1..100`，soft=80 只在 81..100 标记；
- hard 已满后相同 Key 返回原事实且不重复占用，不同金额返回幂等冲突；
- 跨 Tenant 账户返回 not found，不泄露存在性；
- Request 外键失败时 Account version/reserved、Reservation、Ledger 全部回滚；
- 受控行锁使第一次 CAS 必然冲突：`MaxAttempts=1` 精确封顶，`MaxAttempts=2` 第二次成功并报告 `Attempts=2`。

## 3. 门禁结果

- 预算包单元专项连续 20 轮通过；
- 160 路真实 PostgreSQL 专项连续 20 轮通过，共执行 3,200 次竞争预留并保持每轮严格 100 允许/60 hard 拒绝；
- 常规与 integration build tag 的 golangci-lint：0 issue；
- instrumented budget 包合并单元与真实 PostgreSQL counter 后覆盖率 82.0%；
- `scripts/dev.ps1 -Action test-integration`：真实 PostgreSQL/Redis 完整集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、迁移顺序、Actions 语法和本地密钥扫描全部通过；
- 实现提交为 `cfb3507`。GitHub Actions [`30782615085`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30782615085) 的 `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 全绿；Linux race、CI PostgreSQL 160 路原子预留、Migration `11 → 10 → 11` 和密钥扫描均明确通过。
