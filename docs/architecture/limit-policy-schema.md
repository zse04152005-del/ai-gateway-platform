# 多级 LimitPolicy Schema

> 状态：Implemented
>
> 日期：2026-07-31
>
> 对应任务：P09-T01 / 迁移 `000010_create_limit_policies`

## 1. 策略形状

LimitPolicy 同时描述 RPM、TPM 和并发三类资源。每类资源有两个独立边界：

- `soft`：允许请求继续，但必须产生可观测告警事实；本任务只定义语义，不提前实现告警。
- `hard`：拒绝新的资源占用；后续限流器只能消费解析后的硬边界。

领域对象采用以下固定形状：

```json
{
  "rpm": {"soft": 80, "hard": 100},
  "tpm": {"soft": 80000, "hard": 100000},
  "concurrency": {"soft": 8, "hard": 10}
}
```

平台层必须提供全部六个正整数。Tenant、Project、Key 层允许稀疏覆盖；每个 `NULL` 只继承父层同名边界，不会把整组资源一起替换。显式空策略无有效意图，因此模型和数据库都拒绝。

所有值限定为 `1..9007199254740991`（`2^53-1`）。该上限保证后续 Redis Lua 在 IEEE-754 数字表示下仍能精确执行整数比较；零不表示无限，无限策略必须通过足够大的显式平台值表达。

## 2. 唯一解析顺序

```text
Platform -> Tenant -> Project -> Key -> Effective
```

`internal/limitpolicy.Resolve` 是唯一继承实现：

1. 从完整 Platform 策略建立六个有效边界和 `platform` 来源。
2. 按顺序应用 Tenant、Project、Key 的非 NULL 字段。
3. 为每个 soft/hard 边界分别保留最终来源。
4. 对最终组合再次验证 `0 < soft <= hard <= 2^53-1`。

单个子策略内 `soft <= hard` 并不足以保证继承后安全。例如 Platform RPM soft=80，而 Project 只把 hard 覆盖为 70，最终组合无效并 fail closed。P09-T02 及之后的本地/Redis 限流器不得重新实现继承，也不得直接消费稀疏策略。

## 3. 持久化与租户隔离

`app.limit_policies` 是 Tenant 拥有、带乐观锁版本和审计字段的稀疏策略表。六个边界使用 nullable `bigint`，并由 CHECK 强制：

- 至少一个边界非 NULL；
- 每个非 NULL 值位于精确整数范围；
- 同一行同时提供某组 soft/hard 时，soft 不大于 hard；
- `policy_ref` 在 Tenant 内唯一且只允许安全标识字符；
- 状态仅为 `active/disabled`，禁用时间与状态一致；
- `version > 0`，审计 Actor 和时间有效。

Tenant、Project、VirtualKey 分别新增 `limit_policy_id`。引用全部以 Tenant 复合外键建立：

- Tenant：`(id, limit_policy_id) -> limit_policies(tenant_id, id)`；
- Project/Key：`(tenant_id, limit_policy_id) -> limit_policies(tenant_id, id)`。

因此即使知道另一个 Tenant 的 Policy UUID，也不能越权绑定。被引用策略使用 `ON DELETE RESTRICT`，避免静默改变运行中继承结果。

## 4. 兼容迁移

这是 expand/migrate/contract 的 expand 阶段：

- Tenant/Project 的旧 `quota_policy_ref` 暂时保留；
- VirtualKey 的旧 `limits` JSONB 暂时保留；
- 同一实体不能同时设置旧字段和新的 `limit_policy_id`；
- `limit_policy_id=NULL` 表示该层不绑定策略并继承父层。

后续控制面会把旧引用迁移为强引用；确认没有旧 Reader/Writer 后，再通过独立迁移删除过渡字段。不能在本迁移内把不透明旧引用猜测映射为新策略。

## 5. 验证

单元测试覆盖逐字段继承、来源追踪、边界值、空子层和跨层不安全组合。`TestLimitPolicySchemaConstraints` 在真实 PostgreSQL 中覆盖稀疏 NULL、三层强引用、重复引用、非法范围、soft/hard 反转、跨租户绑定、旧新字段互斥、生命周期和受限删除。
