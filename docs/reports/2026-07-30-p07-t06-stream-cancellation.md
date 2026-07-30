# P07-T06 客户端取消与上游释放验收报告

- 日期：2026-07-30
- 范围：流式客户端取消、Adapter/Body 释放、传播耗时、连接与 Goroutine 泄漏
- 结论：实现完成，等待正式仓库门禁与 GitHub Actions

## 1. 实现结果

`GuardedStream.Next` 现在在进入阻塞读取前注册 client Context `AfterFunc`。取消发生时，回调先取消本次读取 Context，再以 first-terminal-wins 写入 Controller 终态，并通过唯一 `ChunkStream.Close` 关闭上游 Body。即使 Adapter 忽略 per-call Context，只要履行并发 Close 契约也会被解阻塞。

为消除“Next 已返回但并发 Close 尚未完成”的可观测窗口，terminal return path 会调用相同的 `sync.Once`；当另一个 Goroutine 正在执行 Close 时，它等待 Close 完成。因此调用方收到取消错误时，本地释放证据已完整。

`TimeoutSnapshot` 新增：

- `CancellationObservedAt`；
- `UpstreamReleasedAt`；
- `CancellationPropagation`。

字段只描述本地控制面，不包含取消 cause 文本、URL、Provider 或用户内容，也不把“关闭 TCP”错误宣传为“Provider 保证停止计费”。

## 2. 真实链路验证

新增测试通过真实 `httptest.Server`、共享 `upstreamhttp.Client.DoStream`、Mock Adapter、共享 SSE Decoder 与 TimeoutController 建立完整上游链路。Provider 发出 message-start 和首个 Content Delta 后阻塞，测试取消客户端 Context，并断言：

1. 阻塞的 `GuardedStream.Next` 在 1 秒 SLO 内返回；
2. 错误同时保留 `context.Canceled` 与可信 CancelCause；
3. Provider Handler 的真实 Request Context 在 1 秒内结束；
4. Next 返回时 Snapshot 已记录非负传播耗时和释放时间；
5. 首模型事件事实仍为 true，取消不会伪装成 timeout；
6. 重复 Close 不增加释放次数。

测试每轮执行 25 条真实 HTTP 流，追踪 net/http `ConnState`，批次结束必须归零；同时记录 Runtime Goroutine 基线，关闭 idle pool 并 GC 后必须回到固定噪声窗口。该完整场景重复 20 轮（共 500 次取消）稳定，streaming 全套重复 20 轮稳定，包覆盖率 87.1%。

最终 SHA-256 同步、双 lint、完整门禁、PostgreSQL 回归、提交和 GitHub Actions 证据只在远端全绿后写入开发执行清单。
