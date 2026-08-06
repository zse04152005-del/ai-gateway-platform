# P10-T08 Usage Ledger Adjustment 验收报告

- 日期：2026-08-06
- 范围：追加式修正、可信 Scope、并发幂等、审计证据、signed 成本聚合
- 结论：实现、本机 Migration 演练、完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

实现提交 `2cf95d6` 新增 `internal/meteringadjustment`。调用方提供可信 Tenant/Project Scope、稳定 Event ID 与幂等键、原始 Ledger Event ID、绝对修正后数量和金额；Writer 锁定原始行，读取此前所有修正并计算本次 signed delta，再追加一条 `source=adjustment` 的新 Ledger 行。原始行与所有历史 Adjustment 仍由数据库触发器禁止 UPDATE/DELETE。

绝对结果而不是重复 delta 作为命令事实，因此同一幂等键重试不会再次扣减或增加。相同键与相同事实返回 replay；相同键或 Event ID 携带不同事实返回 conflict；修正后结果已经相同时返回 no-change。数量与金额都能修正或完全归零，但数据库要求每次修正后的有效数量和金额保持在 `0..2^53-1`。

## 2. 归属与审计边界

Migration 20 增加 `adjusts_event_id`、幂等键、有限 `origin`、原因码、内容无关的外部 reference、actor 和修正后绝对结果。允许的来源只有 manual、provider reconciliation 和 system repair；reference 只保存 ticket、batch/item 或 incident 标识，不保存账单文件、Prompt、Response、Credential 或自由文本证据。

数据库按原始 Ledger 行锁串行化并发修正，并再次核对 Tenant、Request、Attempt、Token 类型和 PriceVersion 完全相同。Adjustment 只能引用原始 Provider/Estimated/Reconciled 行，不能形成 Adjustment→Adjustment 链；操作者必须与 `created_by` 一致，修正时间不能早于原始事实。Adjustment 不是 Gateway UsageEvent，因此 `event_schema_version` 与 tokenizer/model 证据必须为空。

## 3. 聚合语义

`internal/meteringcost` 现在允许单条 Adjustment 使用负金额，并用任意精度中间值按币种累加全部原始与修正分录。最终 Attempt、Request-level 与 Request 币种总额仍必须位于 `0..2^53-1`；负的最终费用、溢出或跨币种机械相加继续 fail closed。

这使费用查询可以解释原始数量/金额、每次 signed 修正和当前有效结果，而不依赖可变的 Request cost 摘要。P10-T09 将在该不可变事实链上提供按 requestId 的查询 API。

## 4. 本机门禁

- Adjustment 与 signed 聚合单元测试连续 20 轮通过；
- 真实 PostgreSQL Adjustment 专项连续 20 轮通过：16 路相同命令并发严格得到 1 次 insert、15 次 replay；跨 Scope、幂等冲突、no-change、修正链、缺失审计字段和伪造绝对结果均被拒绝；
- 原始 `quantity=100/amount=250` 在三次修正后仍原样保留，追加链的有效结果为 `quantity=90/amount=225`，成本聚合返回 USD 225 micros；
- 完整 PostgreSQL、Redis、Redpanda integration 以及 `scripts/dev.ps1 -Action check` 全部通过；双标签 lint 0 issue、全量单测/构建成功、项目调用路径 0 漏洞、20 个迁移顺序与密钥扫描通过；
- Down 前 Adjustment 为 0 行；完成 `20→19→20`，两个阶段均为 `dirty=false`，恢复后完整 integration 再次通过，最终为 `version=20 dirty=false`。

## 5. 远端证据

GitHub Actions [`31065497445`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/31065497445) 三个 Job 全绿：`go-quality` 通过 Linux race、进程生命周期、lint、构建与漏洞门禁；`migration-integration` 验证 Adjustment 真实 PostgreSQL 场景和 Migration `20→19→20` 生命周期；`config-and-secrets` 通过 YAML 与双重密钥扫描。
