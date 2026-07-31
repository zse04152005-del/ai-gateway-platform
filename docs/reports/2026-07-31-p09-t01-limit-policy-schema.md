# P09-T01 多级 LimitPolicy Schema 验收报告

- 日期：2026-07-31
- 范围：RPM、TPM、并发，Platform/Tenant/Project/Key 继承，soft/hard 阈值
- 结论：实现与本地完整门禁通过；远端门禁待实现提交后回填

## 1. 实现结果

新增 `internal/limitpolicy` 领域模型。平台层必须完整定义六个边界，Tenant、Project、Key 可以逐字段稀疏覆盖；解析顺序固定为 Platform → Tenant → Project → Key。解析结果保存每个 soft/hard 边界的来源，后续本地和 Redis 限流器只能消费统一的 `Effective`，不能自行重写继承规则。

值域固定为 `1..2^53-1`，保证后续 Redis Lua 整数比较精确。单层和最终解析都强制 `soft <= hard`；因此“父层 soft 高于子层新 hard”会 fail closed，而不是带着矛盾策略进入数据面。

## 2. 数据库与兼容迁移

迁移 `000010_create_limit_policies` 新增 Tenant 拥有、带版本/状态/审计字段的 `app.limit_policies`，六个 nullable bigint 分别表达 RPM/TPM/并发 soft/hard 稀疏覆盖。数据库拒绝全 NULL、零、负数、超过 `2^53-1`、本地 soft/hard 反转、重复 Tenant 内引用和非法生命周期。

Tenant、Project、VirtualKey 新增 `limit_policy_id`，全部通过 Tenant 复合外键阻止跨租户引用，被引用策略禁止级联删除。旧 `quota_policy_ref` 和 Key `limits` 按 expand/migrate/contract 暂时保留，但 CHECK 禁止旧字段与新强引用同时存在，避免双事实源。

## 3. 自动化覆盖

单元测试覆盖：

- 六个边界的逐字段继承和 Platform/Tenant/Project/Key 来源；
- Platform 不完整、显式空子层、零、超过精确整数上限；
- 单层 soft/hard 反转和继承后反转；
- Policy UUID、引用、状态、版本、Actor、时间与 disabled 生命周期。

真实 PostgreSQL 的 `TestLimitPolicySchemaConstraints` 覆盖：

- Tenant/Project/Key 三层有效强引用和稀疏 NULL 存储；
- 最大精确整数、全 NULL、零、负数、越界和 soft/hard 反转；
- Tenant 内引用唯一、跨 Tenant 同名允许、非法引用格式；
- Tenant/Project/Key 三种跨租户绑定拒绝；
- 三种旧/新字段互斥、被引用策略 RESTRICT 删除和禁用生命周期。

## 4. 本地门禁结果

- `go test -count=20 -cover ./internal/limitpolicy`：连续 20 轮通过，覆盖率 97.7%；
- `scripts/dev.ps1 -Action test-integration`：真实 PostgreSQL 完整集成套件通过；
- 迁移回滚/前滚：`10→9→10`，两端均 `dirty=false`；
- `scripts/dev.ps1 -Action check`：格式、常规与 integration lint、全量单测、构建、漏洞、迁移顺序、Actions 语法和本地高风险密钥扫描全部通过；
- 迁移校验：`count=10 latest=000010_create_limit_policies`。

本机 Windows 为 `CGO_ENABLED=0` 且没有 C 编译器，Linux GitHub Actions 的 `go test -race` 是最终并发门禁。

## 5. 远端门禁

待实现提交推送并等待 GitHub Actions 的 `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 全绿后回填提交和 run 证据。
