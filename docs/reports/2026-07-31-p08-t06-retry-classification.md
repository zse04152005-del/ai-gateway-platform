# P08-T06 重试分类验收报告

- 日期：2026-07-31
- 范围：错误分类、不可逆首输出边界、Attempt/总时间/费用预算、`Retry-After`、重复计费风险
- 结论：工作副本实现和专项验证完成，等待正式仓库门禁与 GitHub Actions

## 1. 实现结果

新增独立 `internal/retry` 纯分类器，输出 `no_retry`、`retry_allowed` 或 `different_deployment_only`。分类器不选择 Deployment、不执行等待、不创建 Attempt、不写健康状态，也不持有原始错误，因此 P08-T07 可以在不重复解释错误的情况下消费统一策略。

认证、权限、参数、上下文长度、内容策略、Caller Cancellation/Deadline、未知错误和本地 Adapter 配置全部不重试。429、Capacity、Timeout 和明确临时 5xx 只有在 Adapter 给出可信 retryable 事实后才能进入预算门禁；Transport、Protocol 与真实 `TimeoutFailure` 采用稳定类型判断，不解析错误字符串。

## 2. 不可逆和成本边界

已产生客户端可见模型内容、推理或工具 Delta 时，`model_output_started` 具有最高优先级，任何错误或剩余预算都不能重新打开重试。首 Token 超时且未输出时只允许切换另一个 Deployment；no-progress 和首输出后的总超时保持部分失败。

请求提交状态显式区分 `not_submitted/submitted/unknown`。Timeout、临时 5xx 和 Transport 在不能证明未提交时收窄为“仅换 Deployment”，避免对同一异常目标重复调用；额外费用必须由上层显式允许，零值不会隐式放行。

## 3. 时间与放大控制

当前 Attempt 达到最大值立即停止，硬上限为 32。所有 Attempt 共享同一 Deadline；下一次执行必须留下正的最小有效窗口。Provider `Retry-After` 与最小窗口之和不能超过剩余总时间，相等边界允许。过去的 Deadline 产生可复盘的 `total_deadline_exhausted`，而不是模糊输入错误。

## 4. 安全证据

Decision 只包含有限枚举、Attempt 计数、向上取整的毫秒值和布尔状态。序列化测试使用含私有 Provider 文本的未知错误，确认 JSON 不含错误文本、Provider Body、内容或 cause。非法/缺失提交状态、费用许可、时钟、Attempt 和窗口全部稳定 fail closed 为 `retry.ErrInvalid`。

## 5. 专项验证

测试覆盖全部 Normalized Error 类别、retryable true/false、Transport/Protocol/Adapter/Caller 类型、提交三态、首 Token/首输出后超时、Attempt/费用/Deadline/最小窗口/`Retry-After` 精确边界、非法输入、安全 JSON 和 64 路并发确定性。

专项测试连续 10 轮通过，语句覆盖率 97.9%。最终双仓 SHA-256、完整门禁、真实 PostgreSQL 回归、提交、推送与 GitHub Actions 证据只在远端三个 Job 全绿后写入开发执行清单。
