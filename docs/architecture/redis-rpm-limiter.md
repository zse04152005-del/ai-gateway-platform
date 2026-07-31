# Redis 原子 RPM 限流

> 状态：Implemented
>
> 日期：2026-07-31
>
> 对应任务：P09-T03

## 1. 一致性目标

`internal/limits.RedisRPMLimiter` 对 Platform、Tenant、Project、VirtualKey 四层 RPM 执行一次 Redis Lua admission。Lua 先读取并验证四个字段，再按 canonical Scope 顺序检查全部 hard，只有全部允许才统一 `HINCRBY`；任一层拒绝时四层计数都不变。

调用只接受 P09-T01 的完整 `limitpolicy.Effective` Binding。Scope 必须构成同一条可信 Tenant/Project/Key 祖先链；稀疏策略、客户端自报身份和未校验 hard 值不能进入脚本。

## 2. 窗口 Key 与字段

默认 Key 为：

```text
agw:limits:rpm:v1:{rpm}:<redis-minute>
```

`{rpm}` 是显式 Redis Cluster hash tag。一个服务器分钟的所有四层计数保存在同一 Hash 中，字段为：

```text
platform
tenant:<tenant-uuid>
project:<tenant-uuid>:<project-uuid>
key:<tenant-uuid>:<project-uuid>:<key-uuid>
```

同一 Lua 只访问一个 Key，因此没有跨 Slot 部分提交。代价是 Platform 字段和分钟 Hash 是全局热点；MVP 优先正确性，P17/P18 压测达到单 Key 瓶颈后再评估按独立限流域分片，不能在没有跨分片语义设计时提前拆散四层原子性。

字段只含非秘密 UUID，不含 VirtualKey 明文、Prompt、响应或 Provider 信息。

## 3. 服务器时钟与分钟边界

Gateway 用本地 UTC 分钟构造第一次 Key，Lua 在任何读写前调用 Redis `TIME`：

1. 本地窗口与 Redis 窗口相同：继续原子检查。
2. 不同：返回 Redis 窗口、当前毫秒和 reset 毫秒，不读取或修改旧 Key。
3. Go 改用服务器窗口 Key 重试，默认最多 3 次。

因此正常路径只有一次 Redis 往返；主机时钟偏差或恰逢分钟切换也不会写错窗口。若连续三次跨边界则返回明确错误并 fail closed，不进行 Provider 调用。

Lua 使用服务器绝对毫秒执行：

```text
PEXPIREAT(window-key, next-minute + 5s retention)
```

默认额外保留 5 秒用于迟到诊断，TTL 不会因每次请求重新延长。允许配置的 retention 为 `1ms..1min`。公开的 hard RetryAfter 只到下一分钟，不包含内部清理保留时间。

## 4. 结果与故障语义

允许结果返回四层新计数、Redis ServerTime、ResetAt 和 WindowKey；Go 依据 Effective soft 生成 `SoftExceeded`，不会让脚本发送告警。拒绝结果返回第一个 canonical hard Scope、当前计数、hard 和精确 RetryAfter。

Redis Hash 中已有值必须是 `HINCRBY` 产生的十进制非负整数且不超过 `2^53-1`。非法字符串、负数、越界、未知 RESP 形状、hard 回显不一致或计数超过 Policy 都视为协议/状态损坏并 fail closed；脚本不会修复或覆盖可疑值。

连接错误和 Context 取消原样包裹返回。P09-T10 负责把 hard 拒绝投影为安全 429，把 Redis 基础设施故障投影为不泄露地址的可用性错误；在未明确配置降级策略前，不能把 Redis 故障静默降级为“只看本地即允许”。

## 5. 验证

单元测试覆盖本地/服务器窗口纠正、固定重试上限、canonical hard 拒绝、soft 事实、Key/字段编码、非法 Binding、传输错误和全部 RESP 损坏分支。

`TestRedisRPMAtomicConcurrencyAndTTL` 在真实 Redis 7.4 上以 128 路并发争抢 hard=50，严格得到 50 次允许和 78 次拒绝；四个字段最终都等于 50。测试同时验证 Tenant 层拒绝不会增加 Platform、绝对 PTTL、主机时钟偏移纠正，以及人工损坏字段后 Lua 不修改其他 Scope。
