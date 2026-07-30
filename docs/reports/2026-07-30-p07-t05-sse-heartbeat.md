# P07-T05 可选 SSE heartbeat 验收报告

- 日期：2026-07-30
- 范围：Gateway-owned SSE heartbeat、客户端开关、资源生命周期
- 结论：实现完成，等待正式仓库门禁与 GitHub Actions

## 1. 生产问题

长 TTFT 或模型思考期间，反向代理、负载均衡和客户端 SDK 可能把“暂时没有模型 Delta”误判为闲置连接并提前断开。直接发送假的 `data:` Token 会污染模型内容、首包边界和 Usage；允许客户端指定毫秒级频率又会造成小包、Flush、CPU 与网络放大。

本实现使用固定 SSE comment 保持连接活跃，并把控制面限制为“平台决定频率、客户端只可 on/off”。

## 2. 核心保证

- Wire 内容恒为 `: gateway-heartbeat\n\n`，不接受调用方提供 comment 内容。
- Header 契约为 `X-AI-Gateway-SSE-Heartbeat: on|off`；缺失等同 on，其他值 fail closed。
- 平台 interval 经过 10 ms～5 min 硬边界校验，客户端无法覆盖。
- disabled 模式不创建 ticker、不调用 Writer、不记录统计。
- enabled Runner 不创建隐藏 Goroutine；调用方显式拥有其阻塞生命周期。
- 使用 `sse.Writer.WriteComment`，继承完整事件串行化、逐事件写 deadline、立即 Flush、断开和慢客户端分类。
- Flush 成功后才记录 heartbeat；写失败不会产生虚假成功计数。
- `TimeoutController.RecordGatewayHeartbeat` 只增加无内容计数，不满足首 Token、不重置 no-progress。
- Snapshot 只包含 Enabled/Running/Finished、时间和次数，不保存业务内容。

## 3. 自动化验证

测试以可注入 fake ticker 和时钟避免依赖真实 sleep：

1. 缺失/on/off 偏好、非法 interval、自定义频率、大小写和空白歧义；
2. disabled 模式零 ticker、零 Writer、零 recorder；
3. 两次确定性 tick 只写固定 comment，并在 Timeout Snapshot 中只增加 Gateway heartbeat；
4. 模型首事件、上游事件数和首 Token 状态保持不变；
5. Context CancelCause 立即停止，ticker 恰好释放；
6. sink 失败原样保留稳定分类且不记录成功；
7. recorder 私有失败转换为稳定 `ErrHeartbeatState`，不泄漏内部文本；
8. 重复 Run、nil/typed-nil port、nil clock/factory/ticker 和零时间 fail closed；
9. heartbeat 专项连续 100 轮稳定；`internal/streaming` 总覆盖率 87.2%。

最终 SHA-256 同步、完整门禁、PostgreSQL 回归、提交和 GitHub Actions 证据仅在远端全绿后写入开发执行清单。
