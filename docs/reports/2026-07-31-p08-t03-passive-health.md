# P08-T03 被动健康统计验收报告

- 日期：2026-07-31
- 范围：真实 Attempt 终态分类、滑动窗口统计、低样本保护、延迟分布、Gateway 装配
- 结论：工作副本实现和专项验证完成，等待正式仓库门禁与 GitHub Actions

## 1. 有界滑动窗口

`routing.PassiveHealth` 为每个 Deployment 维护固定数量的时间桶，默认策略 `passive-health/v1` 为：

- Window：2 分钟；
- Bucket Width：5 秒；
- Minimum Health Samples：20；
- Failure Ratio Threshold：50%；
- Maximum Deployments：10,000。

Window、Bucket 数、样本阈值、故障比率和 Deployment 容量都有硬边界；Window 必须由 Bucket Width 整除，Bucket 数限制为 2～720。Deployment Map 达到上限时按 Last Observed Time 做确定性 LRU 淘汰，相同时间用 Deployment ID 打破平局，避免长期运行或目录抖动造成无界内存增长。Counter、Latency Sum 与 Eviction Counter 使用饱和运算，不允许整数回绕。

每次 Snapshot 只合并当前环形窗口内的 Bucket。旧故障样本随时间过期，Deployment 会从 degraded 回到 warmup，而不是仅凭被动流量永久拉黑；P08-T05 再负责 Closed/Open/Half-Open 和受限探测。

## 2. 指标与样本语义

每个物理 Attempt 记录：

- Request Count 与 Success Count；
- HTTP 429；
- HTTP 5xx；
- Provider Timeout；
- Caller Cancellation；
- Other Failure；
- First Token Latency（可选）；
- Total Latency。

TTFT 与 Total Latency 都输出 Count、Average、Maximum 及固定 Histogram 推导的 P50/P95/P99 Upper Bound。Histogram 从 10ms 到 60s 分 12 个有限档，超过 60s 的 Tail 使用窗口内真实 Maximum 作为 Quantile Upper Bound，不存放单请求样本或内容。

健康样本只由 Success、429、5xx、Timeout 构成。Caller Cancellation、认证/配置/协议等 Other Failure 仍计入 Request Count，但既不能凑够 Minimum Samples，也不能进入故障比率分母来稀释 Provider 故障。这解决了两个常见误判：少量冷启动失败立即摘除 Deployment，以及大量客户端主动取消掩盖真实上游失败率。

状态语义：

- `warmup`：Health Sample 未达到 Minimum Samples，始终 Eligible；
- `healthy`：样本充足且 `(429 + 5xx + timeout) / health_samples` 低于阈值；
- `degraded`：样本充足且故障比率达到或超过阈值，`HealthReader.Healthy` 返回 false。

## 3. 真实 Gateway 接线

`cmd/gateway` 不再把 `ActiveCatalogHealth` 当成生产运行态健康，而是创建一个进程级 `PassiveHealth`，同一个实例同时提供：

1. Selector 的 `HealthReader`；
2. `ObservedChatExecutor` 的 `PassiveObserver`。

Observed Executor 包裹当前真实非流式 `proxy.NonStreamExecutor`，按经过验证的 Provider Status/Category 和 Transport Sentinel 分类成功、429、5xx、Timeout、Cancellation、Other Failure，并测量完整 Attempt Latency。客户端 Context 已取消时，Provider 执行仍保留原取消语义；只有本地内存 Observation 使用 `context.WithoutCancel` 完成。

Health Observation 是辅助控制信号，不能反向改变用户已经得到的 Response/Error。如果 Observation 被拒绝，包装器保留原结果并原子增加 `ObservationFailures`。非流式完整响应延迟不冒充 TTFT；后续真实流式执行在首模型事件出现时填充同一 Observation 的 FirstTokenLatency。

## 4. 数据最小化与失败边界

`PassiveObservation` 只包含 Deployment ID、有限 Outcome、Provider HTTP Status、可选 TTFT 和 Total Latency。`PassiveSnapshot` 只包含聚合 Counter、Duration、State、Window 时间和 Eviction Count；没有 Tenant、Key、Endpoint、Secret、Provider Error 文本、Prompt 或 Response。

Observation 会校验规范 Deployment UUID、Outcome/Status 一致性、非负且不超过 24 小时的 Total Latency，以及 `0 <= TTFT <= Total Latency`。nil/取消 Context、零时钟、畸形配置、非法 Status 和未初始化实例均 fail closed；Selector 会把 HealthReader 的异常继续隔离为既有 `ErrHealthUnavailable`。

## 5. 专项验证

测试覆盖：

- Minimum Sample 前 100% Timeout 仍 warmup，达到阈值才 degraded；
- 100 个 Cancellation/Other Failure 不凑样本、不稀释后续 Provider Failure；
- 成功、429、5xx、Timeout、Cancellation、Other Failure 六类精确计数；
- TTFT/Total 的 Average、Maximum、P50/P95/P99 与 >60s Tail；
- Bucket 逐步过期、故障窗口恢复和完全过期；
- 确定性 LRU 两轮淘汰与 Eviction Counter；
- 64 路并发共 12,800 次 Observe/Snapshot/Healthy；
- 默认/非法 Options、Observation、Context、时钟、nil 实例与饱和算术；
- Provider Error/Transport Error 的终态分类；
- 客户端取消后 Observation Context、时钟回拨归零、Observation 失败不改变 Attempt；
- Observed Executor 到真实 PassiveHealth Snapshot 的组件集成；
- `cmd/gateway` 使用同一 Tracker 完成选路与观察装配。

Passive 专项连续 20 轮通过；Routing 包覆盖率 96.3%，Gateway 包覆盖率 81.8%，三包 `golangci-lint` 为 0 issue。最终双仓 SHA-256、完整门禁、真实 PostgreSQL 回归、提交、推送与 GitHub Actions 证据只在远端三个 Job 全绿后写入开发执行清单。
