# P08-T05 熔断状态机验收报告

- 日期：2026-07-31
- 范围：Closed/Open/Half-Open、故障归因、Generation Permit、并发探测、路由/执行接入
- 结论：工作副本实现和专项验证完成，等待正式仓库门禁与 GitHub Actions

## 1. 状态和阈值

`routing.CircuitBreaker` 按物理 Deployment 隔离状态，默认策略为：Closed 连续 5 次可归因故障后 Open 30 秒；冷却到期进入 Half-Open；Half-Open 最多 2 个并发 Permit，需要 2 次成功恢复，任一失败立即重新 Open 并重新计算冷却。Closed 成功重置连续失败；Ignored 只释放 Permit，不增加也不重置 Provider 故障证据。

未知 Deployment 视为 Closed，但第一次真实 Acquire 才分配状态，避免路由扫描制造 Map。默认最多 10,000 个状态；只淘汰无在途 Permit 的 Closed 状态，Open、Half-Open 和在途记录受保护。若全部受保护则 `ErrCircuitCapacity` fail closed，不通过遗忘异常状态换取表面可用性。

## 2. 并发正确性

Selector 需要对所有候选生成可解释过滤报告，因此 `Healthy` 只读状态而不预占 Half-Open 名额；否则未被最终选择的候选会泄漏 Permit。真正选中后，`CircuitChatExecutor` 在 Provider 调用前通过同一状态锁原子 Acquire，严格限制并发。检查后发生竞态时请求安全返回 Half-Open 饱和，不会额外调用 Provider。

Permit 记录 Acquire 时的 Generation，并用 Atomic Bool 保证 exactly-once Completion。每次 Open、进入 Half-Open、重新 Open 或 Close 都推进 Generation；一个 Half-Open 失败重新 Open 后，其他旧 Permit 的迟到成功会被忽略，不能误关新 Circuit 或下溢新一代 in-flight 计数。

## 3. 故障归因和边界

真实完成结果只映射为三类：

- `succeeded`：完整有效 Provider Response；
- `failed`：429、Capacity、Timeout、5xx、Provider Protocol 或 Transport 故障；
- `ignored`：Caller Cancellation、Auth/Permission、Invalid Request/Context、Content Policy、不可重试 Unknown、本地 Adapter 配置等非 Provider 可用性事实。

Circuit Executor 位于被动观察器外层。Open/Half-Open 饱和时不会调用 Provider，也不会写入被动样本；已 Acquire 的 Attempt 仍由原被动观察和 durable Attempt Recorder 处理。Completion 本地失败只增加 Counter，绝不覆盖业务 Response/Error。公开错误统一为 503 `MODEL_UNAVAILABLE`，不泄漏状态、阈值、Provider 或私有错误。

## 4. 专项验证

测试覆盖：

- Closed 连续阈值、成功重置、Ignored 保持证据；
- Open 精确冷却边界和重新 Open 的新冷却；
- Half-Open 成功恢复、任一失败重开、Ignored 释放槽位；
- 64 路并发抢占下 Permit 数严格不超过 3；
- Generation 过期完成、重复完成和 in-flight 不下溢；
- 状态硬上限、仅 Closed/idle 确定性淘汰和受保护容量 fail-closed；
- Transport/Protocol/Provider/Timeout 与 Cancellation/配置有限归因；
- Circuit 拒绝不调用下层，Half-Open 执行前原子门禁；
- Completion 失败保留原始业务结果；
- 三种内部拒绝统一安全映射 503，不泄漏私有原因；
- Gateway 同时组合 Passive、Active、Circuit 三个 HealthReader。

专项测试连续 20 轮通过；Routing 语句覆盖率 94.2%，Gateway 82.2%，专项 lint 为 0 issue。最终双仓 SHA-256、完整门禁、真实 PostgreSQL 回归、提交、推送与 GitHub Actions 证据只在远端三个 Job 全绿后写入开发执行清单。
