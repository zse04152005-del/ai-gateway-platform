# TPM 预估、预留与结算

P09-T04 在 P09-T02 本地快速保护之后增加全局 TPM 预留与结算。默认 TPM 口径只包含主 `input_tokens + output_tokens`；Cache Read/Write、Reasoning、Audio Input/Output 保持独立，不能在没有价格或计量策略时重复相加。

## 1. 预估契约

`PlanTPMReservation` 接收一个显式 `InputTokenEstimator`、规范化请求和所选 Deployment 的 `capabilityMaximumOutputTokens`：

1. 请求必须先通过 `adapter.NormalizedRequest.Validate`；
2. 输入估算必须返回正整数、稳定方法名和版本；
3. 请求给出 `MaxOutputTokens` 时使用请求值，但不能超过 Deployment Capability；
4. 请求未给出时使用调用方显式传入的 Deployment Capability，限流层不猜默认值；
5. `ReservedTokens = estimated input + maximum output`，总值限制在 Redis/Lua 可精确表示的 `2^53-1`；
6. 计划固定标记 `Estimated=true`，不能作为 Provider 账单事实。

P10-T07 才会提供模型 Tokenizer 与缓存；当前契约允许注入保守估算器，同时用 `EstimatorMethod`、`EstimatorVersion` 保留可复现证据。内置 `NormalizedJSONByteEstimator` 对通过校验的规范化请求 JSON 字节加固定 framing allowance，受所选模型最大输入上限约束，方法固定标为 `normalized-json-byte-bound/v1`，明确不是精确 Tokenizer。估算器错误或非法结果 fail closed，不能退化为只预留输出。

同一个 `ReservedTokens` 可传入本地 `LocalLimiter.Request.EstimatedTokens` 做单实例早期保护，再传给 Redis 全局层；本地分钟计数保持悲观值并自然过期，Redis 是完成后释放差额的全局权威。

## 2. Redis 原子预留

`RedisTPMLimiter.ReserveTPM` 使用 Redis `TIME` 决定 UTC 分钟。版本化 `{tpm}` Hash 同时保存 Platform、Tenant、Project、VirtualKey 四个计数和 `reservation:<id>` 状态：

- Lua 在任何写入前检查服务器分钟、四个 TPM hard、现有十进制计数和 Reservation ID；
- 四层全部允许才统一 `HINCRBY reserved`，任一层拒绝不会部分增加祖先或后代；
- 同一分钟内相同 ID、Scope 指纹和 reserved 的重试返回幂等成功；不同 Scope 或 reserved 冲突 fail closed；
- Scope 指纹是四个 tenant-qualified Redis Field 的 SHA-256，结算不能替换层级；
- `PEXPIREAT` 固定为原分钟 ResetAt 加 Settlement Retention，默认一小时、最大 24 小时，重试和结算都不滑动续期；
- soft 只产生结构化事实，hard 才拒绝。

本地时钟只预测第一次 Key。Lua 发现分钟不一致时先返回服务器窗口且不写入，Go 最多重试三次。Redis 传输错误、损坏字段和未知 RESP 都是基础设施/协议错误，不伪装为业务限流，也不绕过 hard。

## 3. 终态结算

调用方从终态 `NormalizedUsage` 生成 `TPMActual`，要求 Input 和 Output 两个主维度都 Present。正常完成使用 Provider 终态事实；失败或取消只能在已经形成终态、且调用方明确接受当前已知 Usage 时结算，`Complete=false` 会被原样保留，不能伪装成完整账单。

`SettleTPM` 必须携带预留返回的 Handle，并始终写回 Handle 中的原分钟 Key：

- `actual < reserved`：四层同时扣减差额并返回 `ReleasedTokens`；
- `actual == reserved`：只关闭 Reservation，不改变计数；
- `actual > reserved`：四层同时补记超额并返回 `OverageTokens`。计数允许暂时超过 hard，后续 admission 会被拒绝，不能少记真实使用量；
- 相同 actual 的重复结算返回幂等成功；同一 ID 的不同 actual、Scope 或 reserved 返回冲突且不修改计数；
- 原 Key 已过期时返回 `ErrRedisTPMReservationExpired`，绝不把差额应用到当前分钟；
- 任一计数损坏、释放会下溢或补记会越过 `2^53-1` 时，全事务 fail closed。

Reservation ID 应为每个 Gateway Request/计量单元生成的稳定唯一 ID。一个上游重试 Attempt 不应重复预留最大输出；多 Attempt 的最终 Usage 聚合由执行层按既有 Provider Final → Chunk Provider → Estimate 事实轨选择后，只结算一次。

## 4. 并发与故障性质

预留和结算各自是一条 Lua，单次操作内四层全有或全无。Reserve 与 Settle 并发时由 Redis 串行执行；Reservation 状态从 `r:<reserved>:<scope-fingerprint>` 单向变成 `s:<reserved>:<actual>:<scope-fingerprint>`。没有后台补偿或静默重试：调用方可用同一 ID/Handle 安全重试，超过 Retention 的异常终态必须进入审计和后续对账流程。
