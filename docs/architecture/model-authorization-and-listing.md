# 项目/Key 模型白名单与 `/v1/models`

> 状态：Implemented
> 日期：2026-07-30
> 对应任务：P04-T06
> 迁移：`000005_create_project_model_allowlist`

## 1. 授权集合

`GET /v1/models` 返回以下集合的交集，而不是简单列出目录：

```text
可信 Tenant/Project
  ∩ Project active allowlist
  ∩ Key allowed_models（只允许收窄）
  ∩ LogicalModel active
  ∩ 至少一个 active Binding + Deployment + Provider
```

任何供应商代码、物理模型名、Deployment ID、Endpoint、其他租户 ID 和路由权重都不会进入响应。

## 2. 项目白名单

`app.project_logical_models` 使用 `(tenant_id, project_id, logical_model_id)` 主键，并以两个复合外键同时约束：

- `(tenant_id, project_id) -> projects(tenant_id, id)`；
- `(tenant_id, logical_model_id) -> logical_models(tenant_id, id)`。

因此即使知道另一个租户的 LogicalModel UUID，也不能把它授权给当前项目。记录支持 active/disabled、乐观版本与审计字段，删除采用 RESTRICT。

## 3. Key 白名单三态

Virtual Key 的 `allowed_models` 有意区分三种状态：

| Key 值 | 语义 |
|---|---|
| `NULL` | 继承项目白名单 |
| 空数组 `[]` | 显式拒绝所有模型 |
| 非空数组 | 只能从项目白名单中进一步收窄 |

P04-T06 的真实数据库测试发现并修复了空 slice 深拷贝为 nil slice 后被 PostgreSQL 写成 `NULL` 的问题。当前 `virtualkey` 与 `keyauth` 深拷贝都保留“nil 指针 / 非 nil 空 slice”的区别，并有单元与端到端回归测试。

Key 中不存在、属于别的项目或当前不可用的模型名被安全忽略，绝不会扩张项目授权。

## 4. 可用性定义

一个逻辑模型只有在以下全部成立时才可见：

- Tenant、Project active；
- 项目授权记录 active；
- LogicalModel active；
- 至少一个 Binding active；
- 对应 Deployment active；
- 对应 Provider active。

Capability/Region 兼容性已由 P04-T05 的数据库触发器保证。运行态熔断、健康探测、预算和容量不参与目录可见性；它们由后续路由阶段决定，避免短暂故障让模型列表高频抖动。

## 5. 查询与故障语义

`internal/catalog.PostgresStore.ListAvailable`：

- 必须接收认证中间件生成的 Tenant/Project Principal；
- 将显式 Key 名称规范为小写，保持 Key Schema 的大小写不敏感语义；
- 使用参数化 PostgreSQL 查询；
- 按逻辑模型名确定性排序；
- 超过 1000 个结果时整体 fail closed，不返回静默截断的授权视图；
- 对存储的 Capability JSON 再做类型化解码与领域校验。

公开 HTTP 语义：

| 场景 | HTTP | 错误码 |
|---|---:|---|
| 缺失/无效/禁用 Key | 401 | `INVALID_API_KEY` |
| 非 GET | 405 | `METHOD_NOT_ALLOWED` |
| PostgreSQL、Principal 或目录记录不可可信 | 503 | `MODEL_CATALOG_UNAVAILABLE` |
| 成功但无授权模型 | 200 | `data: []` |

503 不包含数据库地址、SQL、Tenant、Project 或内部错误；响应带 `Retry-After: 1`。成功响应带 `Cache-Control: no-store` 与 `X-Content-Type-Options: nosniff`。

## 6. 端到端验证

`TestModelListAuthorizationIntersection` 使用真实 PostgreSQL 和完整 Bearer 凭据贯通：

1. Virtual Key 签发与摘要持久化；
2. 数据面认证和可信 Principal；
3. 项目白名单；
4. Key 继承、显式空拒绝、非空收窄；
5. Provider/Deployment 可用性过滤；
6. 跨租户伪造 Header 和复合外键越权；
7. `/v1/models` 安全 JSON 响应与方法限制。

测试明确断言响应不含 Provider、物理模型、内部 Endpoint 或其他租户信息。
