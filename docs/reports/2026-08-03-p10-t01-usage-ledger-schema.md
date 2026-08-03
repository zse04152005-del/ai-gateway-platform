# P10-T01 Request/Attempt/Usage Ledger Schema 验收报告

- 日期：2026-08-03
- 范围：Request/Attempt 计量归属、Usage Ledger 追加写入、全局事件幂等与租户隔离
- 结论：实现与本地完整门禁通过；实现提交和 GitHub Actions 证据待补充

## 1. 实现结果

Migration 13 在 Migration 7 已有的 `gateway_requests` 与 `route_attempts` 事实根上新增 `app.usage_ledger_entries`。每条分录保存全局 UUID `event_id`、可信 Tenant/Request、可选 Attempt、Token 类型、精确整数数量、计量来源、观察时间与创建审计；不保存 Prompt、Response、Credential、Secret 或 Provider 原始 Payload。

`(tenant_id, request_id)` 复合外键阻止跨 Tenant 归属，`(request_id, attempt_id)` 复合外键阻止把真实 Attempt 绑定到其他 Request。`attempt_id` 允许为空，使缓存命中等没有物理调用的事实仍能归属于 Request。全局 `event_id` 唯一约束为后续至少一次 Metering Consumer 提供幂等底座。

Usage Ledger 只允许追加：数据库触发器统一拒绝 UPDATE/DELETE 并返回 SQLSTATE `23514`。`quantity` 限定为 `1..2^53-1`；`token_type` 和 `source` 先使用最长 64 字符的小写安全标识符，完整领域枚举留给 P10-T02，价格版本、金额与 Adjustment 引用分别留给 P10-T03/P10-T08。

## 2. 场景覆盖

- Request 级缓存用量允许 `attempt_id=NULL`，Attempt 级 Provider 用量保存真实 Attempt；
- 同一 `event_id` 即使换到其他 Request 也被全局唯一约束拒绝；
- Tenant 与 Request 错配、Attempt 与 Request 错配分别由复合外键拒绝；
- 零数量、超过 `2^53-1`、大小写 Token 类型和含路径字符的来源均被 CHECK 拒绝；
- UPDATE 与 DELETE 均被数据库触发器拒绝，原两条分录和总数量保持不变；
- `information_schema` 检查确认表中不存在 Prompt、Response、Credential、Secret、Payload、Body 或 Content 列。

## 3. 门禁结果

- 真实 PostgreSQL `TestUsageLedgerSchemaConstraints`：连续 20 轮通过；
- `internal/execution` 单元 profile 与完整 Request/Attempt 生命周期、Usage Ledger PostgreSQL profile 合并：84.0% statements；
- `scripts/dev.ps1 -Action test-integration`：完整 PostgreSQL/Redis/进程集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、13 个迁移顺序、Actions 语法和本地密钥扫描全部通过；
- 本机迁移生命周期：确认 `usage_ledger_entries=0` 后完成 `13→12→13`，两端均为 `dirty=false`。

## 4. 远端证据

实现提交和 GitHub Actions `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 结果将在推送后补充。
