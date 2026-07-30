# 进程优雅关闭与连接排空

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P03-T08

## 1. HTTP 关闭顺序

gateway 与 control-plane 共用以下顺序：

1. 收到 SIGINT/SIGTERM 后立即把 readiness 置为 false。
2. 原子进入 draining 状态；已经被中间件接收的请求已在注册表中，尚未注册的晚到请求返回 `503 SERVER_DRAINING`。
3. 所有通过 `httpserver.MarkStreaming(ctx)` 标记的 SSE/长流请求立即收到 `ErrStreamingShutdown` Context cause，开始释放上游连接、缓冲和 Goroutine。
4. 调用 `http.Server.Shutdown` 停止监听、关闭 idle keep-alive，并在 `SHUTDOWN_TIMEOUT` 内等待普通请求完成。
5. 超时后，以 `ErrForcedShutdown` 取消注册表中全部剩余请求，再调用 `http.Server.Close` 强制关闭连接。
6. 只有监听循环和连接排空都结束后，进程才返回。

若监听器在未收到关停信号时异常退出，全部在途请求收到 `ErrServerStopped`，不会留下孤立 Handler。

## 2. 流式 Handler 契约

流式 Handler 必须在开始等待上游或写入 SSE 前调用：

```go
if err := httpserver.MarkStreaming(request.Context()); err != nil {
    // 映射为统一内部错误；不得继续建立上游流。
}
```

只设置 `Content-Type: text/event-stream` 不等于完成注册。显式标记避免 ResponseWriter 包装破坏 `Flusher`、HTTP/2 等可选接口，也保证在首个下游字节前就可取消。

Handler 必须监听 `request.Context().Done()`，并把 `context.Cause` 映射到取消/部分失败语义；不得把关停取消当作 Provider 失败重试到另一个模型。

## 3. 计量 Worker 关闭

- 收到取消后先把 `Connected()` 置为 false，再用独立的 `SHUTDOWN_TIMEOUT` Context 关闭事件总线 Session。
- Session 实现应主动响应 Context；Worker 额外在外层竞争 Close 结果与 deadline，即使错误实现忽略 Context，主关闭流程也会在时限后返回 `context.DeadlineExceeded`。
- 当前 TCP Session 的 Close 是幂等式即时资源释放；后续 Kafka Consumer 必须在 Close 中停止拉取、提交允许的 offset 并释放连接。

## 4. 验证与排障

发布前至少验证：

- 一个普通请求和一个已标记流同时在途：流立即取消，普通请求完成前进程不退出。
- 普通请求超过 deadline：Context 收到 `ErrForcedShutdown`，进程在有限时间返回。
- draining 竞态请求不进入业务 Handler，响应携带统一 Request ID。
- 关闭后 `ActiveConnections()` 返回 `0, 0`。
- Worker Session 忽略 Context 时，主流程仍按 deadline 返回；测试随后释放模拟 Session，避免测试 Goroutine 泄漏。

排障时记录活跃普通/流请求数量和关停耗时，不记录 Prompt、Response 或连接凭据。
