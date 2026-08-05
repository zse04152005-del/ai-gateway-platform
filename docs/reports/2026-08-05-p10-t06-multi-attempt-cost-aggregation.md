# P10-T06 多 Attempt 成本聚合验收报告

- 日期：2026-08-05
- 范围：Request/Attempt 费用重建、Outbox 完整性屏障、失败与部分流费用、币种隔离
- 结论：实现、本机完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

实现提交 `30d156a` 新增 `internal/meteringcost`。请求费用不写入可变 `request.cost` 摘要，而是在 PostgreSQL repeatable-read 只读快照内从不可变 Usage Ledger 即时重建；这样至少一次事件重放不会重复计入，未来追加式 Adjustment 也能进入同一事实链。

结果保留每个 RouteAttempt 的身份、序号、Deployment、终态、Ledger 行数和分币种金额，同时返回 Request 级分录 bucket 与 Request 总额。没有正数量 Usage 的 Attempt 仍以零费用保留；retryable-failed、failed、partial-failed、cancelled 和 succeeded 均不会被状态过滤。

## 2. 完整性与安全边界

读取先以可信 Tenant+Project Scope 验证 Request 已终态，再核对 `attempt_count`、连续 Attempt 序号与全部 Attempt 终态。随后比较该 Request 的 Usage Event Outbox 与 Tenant/Request/Attempt 一致的 Ledger：任一事件尚未定价时返回 `ErrPending`，不会在异步消费窗口内展示少计费用或假零值。

不存在、跨租户和跨项目统一返回 `ErrNotFound`；活动 Request 返回 `ErrNotTerminal`。事实漂移、未知 Attempt、重复 Event、非法币种或总额越过 `2^53-1` 均 fail closed。不同 PriceVersion 可能使用不同币种，因此 Attempt 和 Request 都按三位大写币种分桶，禁止跨币种机械相加。返回内容不读取 Prompt、Response、Credential、Secret、Endpoint 或 Raw Provider Evidence。

## 3. 部分流语义

首模型 Token 之后不允许透明故障切换，因此 partial-failed 与“失败后最终成功”是两个独立场景。多 Attempt 场景把首包前 retryable-failed Attempt 的费用与最终成功 Attempt 相加；部分流场景保留 terminal partial-failed Attempt 的已知 Provider 成本。普通 failed Attempt 即使没有向客户端交付有效结果，只要 Usage 已知也必须入总账。

## 4. 本机门禁

- 纯领域测试连续 20 轮通过，覆盖零费用 Attempt、多币种隔离、Request 级 bucket、重复事件、未知 Attempt、状态漂移与总额溢出；单元覆盖率 42.2%，PostgreSQL 读取主路径由真实集成测试覆盖；
- 真实 PostgreSQL 专项连续 20 轮通过：Outbox 只落一半 Ledger 时返回 pending；retryable-failed 2,500,000 micros 与最终成功 5,000,000 micros 得到 Request 总额 7,500,000 micros；
- partial-failed 流式 Attempt 计入 1,000,000 micros，failed Attempt 计入 500,000 micros；活动 Request 与跨 Tenant/Project Scope 安全拒绝；
- `scripts/dev.ps1 -Action test-integration` 完整 PostgreSQL、Redis、Redpanda 与进程集成套件通过；
- `scripts/dev.ps1 -Action check` 通过模块校验、格式、双标签 lint、全量单测/构建、govulncheck、18 个迁移顺序、Actions 语法与本地密钥扫描；本任务没有 Schema 变更，本机仍为 `version=18 dirty=false`。

## 5. 远端证据

GitHub Actions [`30969540993`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30969540993) 三个 Job 全绿：`go-quality` 通过 Linux race、进程生命周期、lint、构建与漏洞门禁；`migration-integration` 强制执行新增多 Attempt 聚合真实 PostgreSQL 场景并完成 Migration `18→16→18`；`config-and-secrets` 通过 YAML 与双重密钥扫描。
