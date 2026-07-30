# P07-T04 首 Token 与 no-progress timeout 验收报告

- 日期：2026-07-30
- 范围：流式 Attempt 生命周期、首模型事件、停顿和总时限
- 结论：实现完成，等待正式仓库门禁与 GitHub Actions

## 1. 问题与边界

流式 Provider 在返回 HTTP 200 后仍可能长时间不产生模型内容，也可能在输出部分内容后停止发送事件。若只依赖 `http.Client.Timeout`，长响应会被任意截断；若把 HTTP Header、Provider heartbeat 或 Gateway heartbeat 当成首 Token，故障切换又会错误跨过“尚未输出模型内容”的安全边界。

本任务建立四个互不混淆的时间阶段：

| 阶段 | 起点 | 终点/重置 | 稳定分类 |
|---|---|---|---|
| 连接/TLS/Header | 上游请求发送 | 可用 HTTP Header | `upstreamhttp.ErrTimeout`/Transport |
| 首模型 Token | `Attach` 记录 Header | 首个 Content/Reasoning/Tool Delta | `ErrFirstTokenTimeout` |
| no-progress | 首模型 Delta | 每个真实上游 Chunk 重置 | `ErrNoProgressTimeout` |
| 总时限 | Controller 创建 | Attempt 终止 | `ErrTotalStreamTimeout` |

Gateway 自己发送的 heartbeat 只增加无内容计数，不推进首 Token 或 no-progress 时钟。Provider heartbeat 在首 Token 前不能冒充模型输出；首 Token 后它代表真实上游活动，可重置 no-progress，但仍受独立总时限限制。

## 2. 实现

`internal/streaming.TimeoutController` 在上游发送前派生 CancelCause Context。调用方必须按以下顺序使用：

```text
NewTimeoutController
  -> Context -> Adapter.BuildRequest -> upstreamhttp.Client.DoStream
  -> Adapter.OpenStream -> Attach (headers_received)
  -> GuardedStream.Next ...
  -> EOF/error/timeout/cancel -> cancel Context + close ChunkStream/Body
```

Controller 在并发锁内裁决 timer 与 Chunk 到达的竞态；过期原因一旦确定便不可覆盖。关闭响应 Body 后产生的 socket/read 错误不会盖掉 timeout cause。no-progress 每次使用 generation 防止已经停止但正在竞争锁的旧 Timer 错误终止新一轮进度。

`TimeoutFailure` 和 `TimeoutSnapshot` 只记录时间、计数和有限枚举，不保存模型内容、Provider 原始 Event 或 Endpoint：

- 首 Token 前：`HeadersReceived=true`、`ModelOutputStarted=false`、`RetryEligibleBeforeOutput=true`、`PartialFailure=false`；
- 首 Token 后：`ModelOutputStarted=true`、`RetryEligibleBeforeOutput=false`、`PartialFailure=true`；
- total timeout 不授予新 Attempt 预算，即使尚未输出模型内容也不标记可重试。

`upstreamhttp.Client.DoStream` 与普通 `Do` 共享唯一 Transport 和连接池，继续执行相同 URL、重定向和 Header 安全检查。它只移除普通 Client 的固定 whole-body timeout；连接、TLS、响应 Header 和 Header 大小上限保持不变，完整生命周期由 Controller Context 管理。

## 3. 自动化验证

专项测试覆盖：

1. 已收到 HTTP Header、Gateway heartbeat、Provider message-start 与 Provider heartbeat 均不能满足首 Token；
2. 首 Token timeout 同时满足通用 stream timeout、phase sentinel 和 `context.DeadlineExceeded`；
3. 首个模型 Content/Reasoning/Tool Delta 关闭首 Token timer；
4. Provider 真实事件重置 no-progress，Gateway heartbeat 不重置；
5. 首包后 no-progress/total timeout 固定为 partial failure 且不可 failover；
6. total timeout 可在 Header 前取消 Build/Dial Context，并关闭迟到的 Stream；
7. 调用方取消、上游终止、显式 Close 和重复 Attach 均幂等释放；
8. timeout 必须取消 Controller Context，并恰好一次关闭上游 ChunkStream；
9. `DoStream` 在普通 `TotalTimeout` 之后仍可读取已建立流的 Body，证明两类总时限没有混用；
10. 专项测试连续 10 轮稳定，`internal/streaming` 语句覆盖率 86.4%。

Linux race detector、全量测试、双 lint、PostgreSQL 集成回归和 GitHub Actions 结果将在提交前后补充到开发清单证据。
