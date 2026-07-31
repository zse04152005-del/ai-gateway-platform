# Redis 分布式并发 Lease

P09-T05 在 P09-T02 本地快速并发保护之后增加跨实例权威并发限制。一次请求必须同时取得 Platform、Tenant、Project、VirtualKey 四层 Lease；任一 hard 已满时不在任何层新增成员。

## 1. 数据结构与原子性

`RedisConcurrencyLimiter` 为四个 Scope 各使用一个 ZSET，Member 是稳定唯一 `LeaseID`，Score 是 Redis 服务器时间计算的绝对 `expiry_ms`。四个 Key 和一个 Lease Metadata Hash 都使用版本化 `{concurrency}` hash tag，Redis Cluster 下位于同一 Slot，可由单条 Lua 原子处理。

Metadata 保存单向状态 `active → released|expired`、四层 tenant-qualified Scope SHA-256 指纹和当前 expiry。Lifecycle 调用同时携带 ID 和 Scope，Lua 必须验证指纹，不能把另一个 Tenant/Project/Key 的 Lease 续租或释放。

Acquire 的顺序固定为：

1. 用 Redis `TIME` 取得 `now_ms`；
2. 对四层执行 `ZREMRANGEBYSCORE <= now_ms`，回收进程退出或失联留下的过期成员；
3. 读取四层 `ZCARD`，按 Platform→Tenant→Project→Key 检查 hard；
4. 全部允许才在四层 `ZADD LeaseID expiry_ms` 并写入 Metadata；
5. 任一拒绝只保留安全的过期清理，不新增任何层成员。

相同 ID、相同 Scope 且仍 active 的 Acquire 重试返回幂等成功。Scope 不同、已经 released/expired 或 Metadata/任一 ZSET 不一致时 fail closed。soft 只产生结构化阈值事实；hard 拒绝返回该 Scope 最早 live Lease 的真实过期时间和 RetryAfter。

## 2. 正常、失败与取消释放

所有终态路径使用同一个 `Release(ctx, handle)`：成功响应、Provider 错误、Gateway 错误和客户端取消都必须执行。下游请求 Context 可能已经取消，因此调用方应在有界、独立的 cleanup Context 中 Release，例如：

```go
cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
defer cancel()
_, err := limiter.Release(cleanupCtx, admission.Handle)
```

Release 在删除前验证四层 Member 的 Score 都与 Metadata expiry 一致，之后四层统一 `ZREM` 并把状态改为 `released`。相同 Handle 的重复 Release 在 Retention 内幂等；不同 Scope 返回冲突；任何成员缺失或 Score 异常都不部分释放。

## 3. 长请求续租与进程退出

默认 LeaseDuration 为 30 秒，允许范围 100 毫秒～10 分钟。长请求必须在到期前调用 `Renew`；建议 Heartbeat 间隔不超过 LeaseDuration 的三分之一并设置抖动。Renew 用 Redis `TIME` 重新计算 `now + LeaseDuration`，验证四层仍完整后统一更新四个 Score 和 Metadata，released/expired Lease 不能复活。

进程异常退出时没有 Release，也不会再 Renew。Lease Score 到期后不再代表有效容量；下一次 Acquire/Renew/Release 会在同一 Lua 内回收过期 Member。ZSET 和 Metadata 的绝对 TTL 固定为最新 expiry 加 Retention，默认 Retention 五分钟、最大一小时，用于终态幂等和诊断，不依赖后台 Reaper 才能恢复容量。

## 4. TTL 与故障语义

Scope ZSET 可能同时包含多个不同 expiry 的 Lease。Acquire/Renew 的新 expiry 来自单调前进的 Redis 服务器时间，并把 Key 的 `PEXPIREAT` 推到 `new expiry + retention`；Release 不滑动 TTL。Metadata 与 Scope Key 使用相同绝对上界。

Redis 连接错误不是业务并发拒绝，调用方不得绕过全局 hard。非法 RESP、损坏 Metadata、孤儿 Member、Score 不一致和不安全整数都返回 `ErrRedisConcurrencyProtocol`。Lease 已超过 TTL 返回 `ErrRedisConcurrencyLeaseExpired`；ID/Scope 或终态冲突返回 `ErrRedisConcurrencyLeaseConflict`。

本地 `Lease.Release` 应在全局 Acquire 失败时立即回滚；全局 Lease 成功后，终态清理顺序建议先 Release Redis，再 Release 本地。无论顺序如何，本地允许都不能代替 Redis 权威 admission。
