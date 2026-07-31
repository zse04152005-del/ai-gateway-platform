# P08-T09 故障风暴请求放大验收报告

- 日期：2026-07-31
- 范围：并发故障、最大 Attempt、共享 Deadline、额外费用门禁、顺序重试
- 结论：实现与本地完整门禁通过，GitHub Actions 证据在远端全绿后补记

## 1. 放大上界

新增 `TestConcurrentFailureStormAmplificationIsStrictlyLinear`，让 64 个请求同时进入三 Deployment 全故障场景。每个请求只允许一个顺序编排循环，最大 3 个 Attempt；测试用原子计数和 requestId 活跃集合同时观察 Selector、Provider、Attempt、RouteDecision 与 RetryDecision。

实测结果严格为：

- 64 个请求 × 3 次上限 = 192 次 Selector、Provider 调用和 Attempt 创建；
- 192 条 RouteDecision 与 192 条 RetryDecision，不遗漏最终 `no_retry`；
- 128 个中间 `retryable_failed` 和 64 个终态完成；
- 同一个 requestId 的 Provider 调用重叠数为 0；
- 全局 Provider 并发不超过入站请求数 64。

因此单请求工作量是 `O(maxAttempts)`，总工作量是 `O(requests × maxAttempts)`；没有递归、分支 fan-out 或 Adapter 内部隐藏重试造成指数放大。

## 2. 总时间边界

`TestFailureStormSharedDeadlineStopsBeforeLargeAttemptLimit` 把 `MaximumAttempts` 提高到 32，并用单调推进时钟模拟共享总预算消耗。固定目标每次重试先产生“排除旧目标后无候选”，再按 `retry_allowed` 显式复用；最终在第 3 个 Attempt 形成 `no_retry/deadline_exhausted`。

该链路精确产生 3 次 Provider 调用、3 个 Attempt、5 次 Selector/RouteDecision 和 3 条 RetryDecision，证明初选、无候选评估和固定目标复用都共享同一 Deadline，而不是每次 Attempt 重置完整时间预算。

## 3. 费用与计量边界

`TestFailureStormCostDenialStopsBeforeSecondPhysicalCall` 在最大 32 次配置下传入 `AdditionalCost=denied`。Transport 故障后分类器记录 `no_retry/additional_cost_not_allowed`，编排器只创建并完成第 1 个 Attempt，不执行下一次 Selector 或 Provider 调用。

每个已发生的物理调用仍有 Attempt 与 retry 决策事实；拒绝额外费用不会抹掉首个调用，也不会被故障风暴绕过。

## 4. 本地验证

- 三个故障风暴专项测试连续 20 轮通过；
- Gateway/Retry/Routing/RouteDecision 相关包完整回归连续 20 轮通过；
- Gateway 语句覆盖率 81.1%；
- 常规与 integration lint 均为 0 issue；
- `scripts/dev.ps1 -Action check` 完整通过；
- `scripts/dev.ps1 -Action test-integration` 在真实 PostgreSQL 上全量通过。

本机 Windows Go 环境为 `CGO_ENABLED=0` 且无 C 编译器，不能执行本地 `-race`；Linux GitHub Actions 的 `go-quality` race detector 是本任务必须等待的最终并发门禁。
