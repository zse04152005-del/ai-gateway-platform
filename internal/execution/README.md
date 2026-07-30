# Execution Records

`execution.PostgresRecorder` 是数据面请求与物理上游尝试的事实边界。它不保存 Prompt、响应内容、Virtual Key 明文、Provider Secret、Endpoint 或 Raw Usage Evidence。

核心保证：

- Correlation Request ID 成为 GatewayRequest 主键，并保存可信 Tenant/Project/VirtualKey、Logical Model 与 Trace/Span 关联。
- 每次物理调用先创建独立 UUID RouteAttempt；`(request_id, attempt_no)` 唯一，重试/故障切换不能覆盖旧 Attempt。
- StartAttempt 在同一 PostgreSQL 事务中把 Request 切到 running、增加 attempt_count，并记录 Attempt 的 created→connecting。
- MarkAttemptStreaming 在同一事务中记录 connecting→headers_received→streaming、Provider Request ID 和首个客户端可见模型字节；两个状态事件要么全部提交，要么全部回滚。
- CompleteAttempt 在同一事务中记录 Attempt 与 Request 终态；streaming 之后只允许 succeeded、partial_failed 或 cancelled，不能把已经对客户端可见的输出降格成普通/可重试失败。
- 所有更新带 `status + version` Compare-And-Swap。数据库 Trigger 同时强制合法迁移、身份不可变、版本单调和时间关系，旧协程不能覆盖终态。
- 两张 append-only status event 表由 Trigger 自动写入，每个版本都有 from/to/time/reason 证据。
- Usage Summary 只保留 presence-preserving Token Count、来源和完整性；Provider Raw Evidence 在 P10 进入不可变账本。

记录依赖在上游调用前不可用时 Gateway fail closed，防止产生“已经向 Provider 付费但系统完全没有 Attempt 身份”的调用。上游结束后的最终写入失败会返回统一记录依赖错误并留下可恢复的 active Attempt，P13 Reconciler/Runbook 负责识别和收敛。

客户端取消后的终态写入使用脱离入站取消但最多 2 秒的 Context；Request 和 Attempt 均记录 `cancelled/client_cancelled`，同时不延长 Provider I/O 的生命周期。
