# P09-T05 分布式并发限制验收报告

- 日期：2026-07-31
- 范围：Redis 四层 ZSET Lease、Renew、终态 Release、进程退出过期恢复
- 本地结论：实现与完整门禁通过；远程提交和 GitHub Actions 证据待闭环后回填

## 1. 实现结果

新增 `RedisConcurrencyLimiter`。Platform、Tenant、Project、VirtualKey 四个 ZSET 与 Lease Metadata 使用同一个 `{concurrency}` Slot。Acquire Lua 以 Redis `TIME` 为唯一时钟，先清理四层过期成员，再检查四层 Effective Concurrency hard，全部允许才统一写入 `LeaseID → expiry_ms`；任一拒绝不新增任何层成员。

Metadata 保存 active/released/expired 单向状态、expiry 和 tenant-qualified Scope SHA-256 指纹。相同 active Lease 重试幂等，跨 Scope、终态重用、孤儿 Member、Score/Metadata 不一致和异常 RESP 均 fail closed。hard 拒绝返回首个 canonical Scope 及其最早 live Lease 的真实 RetryAfter。

## 2. 生命周期与异常恢复

正常完成、Provider/Gateway 失败和客户端取消统一使用 `Release`，并要求调用方脱离已取消的下游 Context 使用有界 cleanup Context。Lua 在任何删除前确认四层 Member 完整且 Score 一致，之后统一 ZREM；重复终态 Release 在 Retention 内幂等。

长请求用 `Renew` 从 Redis 当前时间延长四层 Score，released/expired Lease 不能复活。进程退出或失联后不再续租，下一次操作通过 `ZREMRANGEBYSCORE` 原子回收过期成员。默认 Lease 30 秒、Retention 五分钟，测试使用 700ms/2s；Scope 与 Metadata 都使用 `expiry + retention` 绝对 TTL，Release 不滑动续期。

## 3. 自动化覆盖

- Options、Lease ID、Binding/Policy、四层层级和 Handle 指纹校验；
- 四层 Key/参数 canonical 编码、soft 事实、hard 首层拒绝及最早过期 RetryAfter；
- active Acquire 幂等、终态/跨 Scope 冲突、Redis 传输与协议错误；
- Renew 四层统一延期、过期/释放不能复活；
- 正常、失败、取消使用独立 cleanup Context 并发 Release，重复 Release 幂等；
- 真实 Redis 64 路争抢 hard=20，严格 20 允许/44 拒绝；
- 10 路并发显式 Release 后四层从 20 精确降到 10；
- 剩余未释放 Lease 模拟进程退出，含一次 Renew 的最长 Lease 到期后，新 Acquire 原子清零并以 count=1 恢复；
- 人工删除 Project Member 后 Release fail closed，Platform Member 不发生部分删除；
- Scope/Metadata PTTL、过期清理与 Redis 错误分类。

## 4. 门禁结果

- `go test -count=20 -cover ./internal/limits`：连续 20 轮通过，合并覆盖率 92.5%；
- `TestRedisConcurrencyLeaseLifecycleAndProcessExpiry` 在真实 Redis 上连续 10 轮通过；
- `scripts/dev.ps1 -Action test-integration`：含真实 PostgreSQL、Redis 的完整集成套件通过；
- 常规与 integration build tag 的 `go vet`、golangci-lint：0 issue；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、迁移顺序、Actions 语法和本地密钥扫描全部通过，迁移仍为 `count=10 latest=000010_create_limit_policies`。

实现提交、GitHub Actions 三 Job 以及清单证据提交将在后续闭环步骤完成并回填。
