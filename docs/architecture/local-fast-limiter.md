# 单实例快速限流

> 状态：Implemented
>
> 日期：2026-07-31
>
> 对应任务：P09-T02

## 1. 作用边界

`internal/limits.LocalLimiter` 是 Gateway 进程内的第一层快速保护，不执行网络或数据库 I/O。它只消费 P09-T01 已解析并校验的 `limitpolicy.Effective`，不会读取稀疏策略或重新实现继承。

每次 admission 必须同时携带同一条可信身份链的四个 Scope：

```text
Platform -> Tenant -> Project -> VirtualKey
```

Scope 会按固定顺序归一化并验证 Tenant/Project 祖先一致。四层 RPM、TPM、并发检查在同一临界区完成；只要一个硬边界拒绝，任何层都不消耗计数。这样不会出现 Platform 已扣减、Tenant 又拒绝的部分提交。

这是单实例早期拒绝，不是全局容量事实。P09-T03/P09-T05 的 Redis 原子层仍必须执行，不能因为本地允许就跳过分布式检查。

## 2. 计数语义

- RPM：每次成功 admission 在四层各增加 1，使用 UTC 固定分钟窗口。
- TPM：调用方提供正整数 `EstimatedTokens`，成功时在四层预占；P09-T04 将增加实际 Token 结算与差额释放。
- 并发：成功时在四层各占一个槽位，并返回进程内 Lease。
- soft：投影用量严格超过 soft 后仍允许，返回结构化 `SoftThreshold`，不在热路径发送通知。
- hard：投影用量超过 hard 时拒绝；RPM/TPM 返回本地窗口 `ResetAt/RetryAfter`，并发不伪造时间型重试承诺。

分钟边界只允许时间向前推进。系统时钟回拨不会把一个已经计数的窗口重新清零，因此最多造成保守拒绝，不会借回拨绕过限额。

## 3. Lease 与释放

成功 admission 返回覆盖四层 Scope 的唯一 Lease。`Release` 使用 `sync.Once`，可以在成功、失败、取消或多个清理路径中重复/并发调用而不会少减或重复减并发计数。RPM/TPM 在分钟窗口内保留，不随请求完成回退。

本地 Lease 只能覆盖正常进程生命周期；进程异常退出会自然清空本地状态。跨实例并发槽位及异常 TTL 由 P09-T05 的 Redis 方案负责。

## 4. 配置热更新

`Replace(version, bindings)` 在 admission 使用的同一锁内发布完整不可变映射，要求版本严格递增，并在发布前验证：

- Binding 数量处于进程内存上限；
- Scope 形状和 UUID 有效；
- Scope 不重复；
- 每个 Policy 都是有效 `limitpolicy.Effective`。

同一 Scope 的分钟计数和在途并发不会因更新重置。降低 hard 不会取消已经获得 Lease 的请求，但新请求会立刻按新版本拒绝；提高 hard 会立即恢复可用容量。删除 Scope 后新请求 fail closed，已有 Lease 仍可安全释放。旧版本、非法快照和缺失任一请求 Scope 都拒绝。

P11 配置分发负责向各 Gateway 推送版本化 Binding；本任务提供可原子热更新的数据面执行器，不在请求热路径查询控制面数据库。

## 5. 可观测与性能

`Admission` 携带配置版本、soft 事件或确定性首个 hard 拒绝；`UsageSnapshot` 只包含 Scope ID、计数、窗口和 Effective Policy，不包含 Prompt、响应或凭据。

Windows/amd64、Intel i7-10700 的本地微基准中，四层 Acquire+Release 约 `2.91µs/op`、`288 B/op`、`1 alloc/op`。该数字只用于发现实现回退，不等同于端到端 SLA；最终网关 P95 必须在 P18 的真实环境压测中测量。

## 6. 验证

自动化测试覆盖四层原子性、soft/hard 精确边界、RPM/TPM 分钟重置、时钟回拨、并发 Lease、幂等释放、版本单调、降低/提高/删除/重加策略、缺失配置 fail closed，以及 64 路争抢 8 个槽位和并发热更新。Linux CI race detector 是最终并发竞态门禁。
