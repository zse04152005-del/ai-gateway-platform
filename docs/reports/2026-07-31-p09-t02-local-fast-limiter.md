# P09-T02 本地快速限流验收报告

- 日期：2026-07-31
- 范围：单实例 RPM/TPM/并发、四层原子 admission、配置热更新
- 结论：实现与完整本地门禁通过；远端门禁待提交后回填

## 1. 实现结果

新增 `internal/limits.LocalLimiter`，对 Platform、Tenant、Project、VirtualKey 四层 Effective 策略执行一次非阻塞、全有或全无的本地 admission。任一 Scope 的 RPM/TPM/并发 hard 拒绝都不会让其他 Scope 部分扣减；soft 超限仍允许并返回结构化事实。

RPM/TPM 使用确定 UTC 分钟窗口，时钟回拨不重置旧计数。成功 admission 返回四层并发 Lease，重复或并发 `Release` 只生效一次。RPM/TPM 拒绝提供本地 reset 时间，并发拒绝不伪造重试时间。

## 2. 热更新与失败边界

`Replace` 以严格递增版本原子替换完整 Binding Map。发布前校验容量、Scope、重复项和 Effective Policy；同 Scope 的计数和在途 Lease 跨版本保留。降低 hard 立即阻止新请求但不取消在途请求，提高 hard 立即放开；删除 Scope 后新 admission fail closed，旧 Lease 仍能释放。

本任务是进程内快速层，不把本地允许冒充分布式容量。Redis RPM、TPM 结算、Redis 并发 TTL 分别由 P09-T03～T05 叠加；P11 的版本化配置分发将调用 Replace，本地热路径不查询控制面数据库。

## 3. 自动化覆盖

- soft/hard 正常、边界和拒绝路径；
- 四层 Scope 顺序归一化、祖先一致性和缺失策略 fail closed；
- Tenant 拒绝时 Platform/Project/Key 无部分消费；
- RPM/TPM 分钟 reset、精确 RetryAfter 和系统时钟回拨；
- 提高/降低 hard、旧版本拒绝、删除/重加 Scope 后状态行为；
- 64 路并发争抢 8 个槽位，严格只允许 8 个且释放后并发归零；
- 32 路 admission 与 99 次版本发布并发执行。

## 4. 专项结果

- `go test -count=20 -cover ./internal/limits`：连续 20 轮通过，覆盖率 98.0%；
- `golangci-lint ./internal/limits`：0 issue；
- Windows/amd64、Intel i7-10700 微基准：约 `2.91µs/op`、`288 B/op`、`1 alloc/op`。

- `scripts/dev.ps1 -Action check`：格式、常规与 integration lint、全量单测、构建、漏洞、迁移顺序、Actions 语法和本地密钥扫描全部通过；
- `scripts/dev.ps1 -Action test-integration`：真实 PostgreSQL 完整集成套件通过；
- 迁移校验保持 `count=10 latest=000010_create_limit_policies`。

GitHub Actions 三个 Job 在实现提交推送后回填。本机 Windows 为 `CGO_ENABLED=0` 且没有 C 编译器，Linux `go test -race` 是最终并发门禁。
