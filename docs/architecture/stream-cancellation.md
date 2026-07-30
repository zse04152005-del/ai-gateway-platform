# 流式客户端取消与上游释放

## 1. 控制流

```text
client disconnect / inbound Context cancel
  -> GuardedStream Next AfterFunc
  -> cancel per-read Context
  -> set immutable controller terminal cause
  -> cancel attempt Context used by BuildRequest + DoStream
  -> ChunkStream.Close (once)
  -> response Body.Close
  -> transport socket cancellation
  -> Provider request Context done
```

每个箭头只传播控制事实，不传播 Prompt、模型内容、Provider Body 或 Endpoint。Adapter 的 `Next` 即使阻塞在网络读取，也必须允许 Context cancellation 或并发 `Close` 解阻塞。

## 2. 返回与释放顺序

取消回调可能与 `Next` 返回、Provider 写入和 total/no-progress timer 并发。Controller 采用 first-terminal-wins：

- timeout 已先终止时保留 timeout 分类；
- client cancellation 已先终止时保留 `context.Canceled` 与可信 CancelCause；
- 终止原因一旦建立不能被 socket close 的 `io.ErrClosedPipe`/read error 覆盖；
- `Next` 的 terminal return path 再调用一次同一 `sync.Once`，若 Close 正在其他 Goroutine 执行则等待它完成；
- 因而 cancelled `Next` 返回时，本地 Adapter/Body release 已完成且可观测。

## 3. 指标事实

`TimeoutSnapshot` 增加三个无内容字段：

| 字段 | 含义 |
|---|---|
| `CancellationObservedAt` | Controller 首次接受取消终态 |
| `UpstreamReleasedAt` | Adapter `Close` 返回 |
| `CancellationPropagation` | 两个本地事实之间的非负耗时 |

这些指标证明 Gateway 已停止读、已取消请求 Context 并已关闭 Body，不宣称 Provider 一定停止计费。远端确认只能通过供应商能力或后续对账获得。

## 4. 验证

真实 HTTP 回归使用共享 `upstreamhttp.DoStream`、Mock Adapter/SSE Decoder 和 `TimeoutController`：Provider 发送 message-start + 首个 Content Delta 后保持连接，客户端取消正在阻塞的下一次读取。测试要求：

- `Next` 在 1 秒内返回并保留 `context.Canceled` 与 CancelCause；
- Provider Handler 的真实 `request.Context()` 在 1 秒内结束；
- Snapshot 在 `Next` 返回时已有观察/释放时间和传播耗时；
- 每个 Stream/Body 恰好一次 Close，重复 Close 幂等；
- 25 次连续取消后活跃上游连接归零；
- GC 后 Goroutine 数回到基线允许的固定运行时噪声窗口；
- 整个矩阵重复执行，Linux CI 使用 race detector 复核。
