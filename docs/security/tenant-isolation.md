# 多租户隔离边界与回归矩阵

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P04-T08

## 1. 安全目标

一个已认证主体只能读取和变更其凭据所属 Tenant/Project 内的资源。攻击者即使掌握另一个租户的 UUID、逻辑模型名、安全前缀或 Provider Secret Reference ID，也不能把这些标识与自己的作用域组合后得到数据、改变状态或推断目标对象是否存在。

当前阶段使用“可信 Principal + Repository 强制谓词 + 数据库复合外键 + 有界缓存”四层防线。PostgreSQL Row-Level Security 尚未启用，因此任何新 Repository 方法都必须把 Tenant/Project 复合谓词作为代码审查和集成测试的硬性要求。

## 2. 不变量

1. Tenant、Project 和 Virtual Key 身份只从通过完整凭据验证的数据库记录生成；客户端提交的 `X-Tenant-Id`、`X-Project-Id`、`X-Virtual-Key-Id` 一律删除。
2. Virtual Key 的直接读取、轮换和吊销必须同时匹配 `tenant_id + project_id + id`。跨作用域与真正不存在返回同一个 `ErrNotFound` 错误族。
3. Project→LogicalModel 授权必须同时匹配 Tenant；把 A 租户与 B 项目拼接进行列表查询只能得到空集合。
4. `/v1/models` 只返回可信 Principal 所属项目允许、Key 允许且物理目录可用的逻辑模型；响应不包含 Tenant/Project UUID、Provider、Deployment、物理模型或 Endpoint。
5. 认证缓存只以数据库全局唯一的安全前缀定位记录。写入键必须与 `Record.Prefix` 相等，缓存输入、输出和 Principal 都深拷贝可变策略，避免跨请求污染。
6. Virtual Key 安全前缀在所有租户间全局唯一，避免相同缓存键映射到两个租户。
7. Provider Secret Reference 以 `provider_id + reference_id` 查询；Deployment 以复合外键绑定同一 Provider 的 Reference，禁止跨 Provider 凭据替换。
8. 错误 Secret 与未知安全前缀对外均返回相同的 `401 INVALID_API_KEY`，响应不得成为 Key 或租户枚举 Oracle。

## 3. 自动化回归矩阵

| 攻击面 | 对抗输入 | 预期结果 | 自动化证据 |
|---|---|---|---|
| 直接 ID | B 租户作用域 + A 租户 Key ID | Get/Rotate/Revoke 均返回与随机 ID 相同的 Not Found；A Key 状态不变 | `TestTenantIsolationBoundaries/direct_IDs...` |
| 列表 | A Tenant + B Project，及反向组合 | 返回空集合，不回退为仅按 Project ID 查询 | `list_queries_reject_mixed...` |
| 缓存 | 修改已返回 Principal；使用 A Key 伪造 B 的 Header | 后续缓存命中仍为 A 作用域和策略；Header 不参与授权 | `cache_and_forged_headers...` |
| Key 创建 | A Tenant + B Project | 复合外键失败并映射为隐藏存在性的 Not Found | `key_creation_and_cache_identity...` |
| 缓存身份 | 在 B 租户插入与 A Key 相同安全前缀 | `virtual_api_keys_prefix_unique` 拒绝 | 同上 |
| 模型查询 | A/B 两个真实 Key 调用 `/v1/models` | 各自只看到本租户逻辑模型；不泄漏物理目录或对方 UUID | `cache_and_forged_headers...` |
| Provider Secret | Provider B + Provider A Reference ID | 与随机 Reference ID 相同的 Not Found | `provider_references...` |
| Deployment 凭据 | Provider A Deployment + Provider B Reference | `deployments_provider_secret_reference_fk` 拒绝，原绑定保持不变 | 同上 |
| 枚举 Oracle | 已知前缀+错误 Secret 与未知前缀 | HTTP 状态、稳定错误码、消息和响应结构完全一致 | `credential_failures...` |

测试使用两个完整 Tenant/Project、两个 Key、两个同等可用的模型链路和两个 Provider，在迁移到最新版本的可抛弃 PostgreSQL 上运行。固定 UUID 仅用于隔离测试数据，完整凭据和 Provider Secret 均在运行时生成且不会写入仓库。

## 4. 执行与 CI

本地真实数据库定向执行：

```powershell
$env:DATABASE_URL = 'postgres://.../disposable_database?sslmode=disable'
go test -count=1 -tags=integration ./tests/integration/... -run TestTenantIsolationBoundaries
```

GitHub Actions 的 `migration-integration` Job 在全量迁移、重复 Up 和版本校验后强制运行该测试。Windows 本地完成单元、普通/Integration lint 和真实 PostgreSQL 测试；Linux CI 额外对普通单元测试运行 race detector。

## 5. 新功能接入检查

新增任何 Tenant/Project 资源时必须同时提交：

1. 带完整作用域的 Locator/Access 类型，不接受调用方只传资源 ID。
2. Repository SQL 中的 Tenant/Project 谓词或等价复合连接。
3. 正确作用域、两个方向的混合作用域、随机不存在 ID 和禁用状态用例。
4. 列表、分页游标、缓存键、批量查询、导出和后台任务的隔离用例。
5. 统一公开错误断言，以及响应体不含目标 UUID、名称、物理目录和内部错误的断言。
6. 若引入 Redis 或其他跨进程缓存，缓存键必须包含不可歧义的作用域/版本，失效事件必须携带同一作用域，并新增跨进程污染测试。

## 6. 剩余风险

- 当前隔离依赖应用查询与数据库复合约束的组合，不把数据库账号直接暴露给租户；未来高敏感管理查询可评估 PostgreSQL RLS 作为额外防线。
- 认证状态、项目状态的缓存一致性仍受显式失效与短 TTL 约束；跨进程失效总线接入前，生产应把 TTL 设为可接受的最短残余风险窗口。
- 本矩阵覆盖 P04 已实现接口。后续新增用量、预算、账单、审计、路由与导出能力时，必须扩展同一矩阵，不能仅复用本阶段结论。
