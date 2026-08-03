# P09-T10 软预算告警与硬预算错误验收报告

- 日期：2026-08-03
- 范围：soft 告警、hard 安全错误、剩余额度、重置时间、有限降级提示与跨租户不泄露
- 结论：实现、本地完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

新增 `budget.LimitNotice`，统一保存 `level`、`remaining_micros`、`reset_at` 和可选 `degradation_hint`。成功 admission 严格超过 soft 时在 `ReserveResult.LimitNotice` 返回 soft 告警；hard 拒绝返回 `budget.HardLimitError`，既保持 `errors.Is(ErrBudgetExceeded)` 兼容，也支持 `errors.As` 读取结构化 Notice。

remaining 使用精确整数：soft 从原 reserve Ledger resulting balances 计算，hard 从拒绝前最新 Account 快照计算；reset 直接使用 Account `period_end`。降级提示只允许 `use_lower_cost_model`、`reduce_max_output`、`wait_for_reset` 三个动作码或空值，自由文本和未知动作在数据库访问前拒绝。

错误字符串保持泛化，Notice JSON 不包含 Tenant、Account、Project、Key、Request 或其他作用域身份。只有 Tenant-qualified Account 读取成功后才创建 hard 元数据；跨 Tenant 的真实 Account ID 仍返回 not found，不能探测他人剩余额度或周期。hard 拒绝不推进 Account version，也不创建 Reservation/Ledger。

## 2. 场景覆盖

- soft=60/hard=100，reserve 60：成功、remaining=40、恰好 soft 无 Notice；
- 再 reserve 10：成功、soft Notice、remaining=30、reset 精确等于 period end，并返回 `reduce_max_output`；
- 再 reserve 31：`errors.Is(ErrBudgetExceeded)` 与 `errors.As(*HardLimitError)` 同时成立，hard Notice 为 remaining=30 和 `wait_for_reset`；
- hard Error JSON/Error string 不包含 Tenant、Account、Request 或任何作用域字段；
- 伪造 Tenant 查询真实 Account 返回 `ErrAccountNotFound`，不能 `errors.As` 为 HardLimitError；
- 数据库最终保持 reserved=70/version=3，Reservation/Ledger 各 2 条，证明 hard 与跨租户拒绝均无副作用；
- Notice 上界、零剩余、零 reset、非法 level/hint、输入非法 hint 和安全 JSON 在单元测试中覆盖。

## 3. 门禁结果

- `go test -count=20 ./internal/budget`：连续 20 轮通过；
- 真实 PostgreSQL `TestPostgresBudgetSoftNoticeAndHardErrorIsolation`：连续 20 轮通过；
- 单元 profile 与 P09-T07～T10 四个真实 PostgreSQL 预算场景 profile 合并：预算包覆盖率 84.4%（455/539 statements）；
- 常规与 integration build tag 的 golangci-lint：0 issue；
- `scripts/dev.ps1 -Action test-integration`：完整 PostgreSQL/Redis/进程集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、12 个迁移顺序、Actions 语法和本地密钥扫描全部通过。

## 4. 远端证据

实现提交为 `bb076df`。GitHub Actions [`30794752455`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30794752455) 的 `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 全绿；Linux race、真实 PostgreSQL soft/hard 隔离、漏洞和密钥扫描均明确通过。
