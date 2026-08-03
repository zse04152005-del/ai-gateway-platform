# P10-T02 Token 类型与计量来源验收报告

- 日期：2026-08-03
- 范围：有限 Token 类型、Provider/估算/对账/修正来源、Go/PostgreSQL 契约一致性
- 结论：实现、本地完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

新增 `internal/metering`，定义九个独立计量维度：`input`、`output`、`cache_read`、`cache_write`、`reasoning`、`audio_input`、`audio_output`、`image_input`、`image_output`。解析严格区分大小写且不自动 trim；`total`、无方向 `audio/image` 与供应商自定义 Meter 均 fail closed。

计量来源直接复用 `adapter.UsageSource`，形成单一 `provider`、`estimated`、`reconciled`、`adjustment` 契约，避免 Provider 规范层与 Metering 层维护两套易漂移常量。Provider/对账 Raw Evidence、估算版本和 Adjustment 原分录引用仍分别由既有 Adapter 契约与 P10-T07/P10-T08 承担。

这些维度不能机械相加：Cache 可能与 Input 重叠，Reasoning 可能与 Output 重叠；本任务只保存原子计量分类，价格单位和估值规则留给 P10-T03。无法映射的 Provider 字段继续保存在受限 UsageEvidence，不能按已知类型静默收费。

## 2. 数据库约束

Migration 14 将 Migration 13 的小写安全字符串约束收紧为与 `internal/metering` 相同的九个 Token 类型和四个来源。约束以 `NOT VALID` 加入后在同一事务内验证历史数据：新写入立即受限，未知旧值会阻止迁移而不会被静默重分类。

Down 只移除有限枚举并恢复 Migration 13 的安全格式约束，不删除 Usage Ledger 表或数据。本机已完成 `14→13→14`，两端均为 `dirty=false`。

## 3. 场景覆盖

- Go 枚举完整顺序、严格 Parse、未知/大小写/空白值拒绝和返回切片防篡改；
- 来源常量与 Adapter 四种 `UsageSource` 完全相同，未知 vendor/inferred/billing 来源拒绝；
- 真实 PostgreSQL 写入 9×4 共 36 个合法组合，所有 Token 类型和来源均可持久化；
- PostgreSQL 拒绝空值、大小写、尾随空格、`total`、无方向媒体类型和供应商自定义 Meter/来源；
- 原有 Usage Ledger 全局 eventId、归属、数量和追加写约束回归继续通过。

## 4. 门禁结果

- `go test -count=20 -cover ./internal/metering`：连续 20 轮通过，覆盖率 100.0%；
- 真实 PostgreSQL `TestUsageTaxonomySchemaParity`：连续 20 轮通过；
- Go 单元与 PostgreSQL profile 合并：`internal/metering` 100.0% statements；
- `scripts/dev.ps1 -Action test-integration`：完整 PostgreSQL/Redis/进程集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、14 个迁移顺序、Actions 语法和本地密钥扫描全部通过；
- 本机迁移生命周期：`14→13→14`，两端均为 `dirty=false`。

## 5. 远端证据

实现提交为 `7464d70`。GitHub Actions [`30801049817`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30801049817) 的 `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 全绿；Linux race、Migration `14→13→14`、真实 PostgreSQL Taxonomy parity、漏洞和密钥扫描均明确通过。
