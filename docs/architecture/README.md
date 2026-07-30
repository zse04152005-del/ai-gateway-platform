# 架构文档

用于记录系统上下文、领域模型、数据流、SLO、技术栈和部署拓扑。架构图必须标记事实源、异步边界与信任边界。

已实现的横切设计：

- [`request-correlation.md`](request-correlation.md)：Request ID 冲突治理与 W3C Trace Context 传播。
- [`tenant-project-schema.md`](tenant-project-schema.md)：Tenant/Project 隔离根、状态、配额引用、审计和唯一约束。
- [`virtual-api-key-schema.md`](virtual-api-key-schema.md)：Virtual API Key 的不可恢复存储、租户隔离、授权覆盖和生命周期约束。
- [`virtual-key-lifecycle.md`](virtual-key-lifecycle.md)：一次性签发、事务轮换、幂等吊销、派生过期和摘要密钥边界。
- [`virtual-key-authentication.md`](virtual-key-authentication.md)：数据面 Bearer 验证、可信 Principal、统一失败语义和缓存失效边界。
- [`model-catalog-schema.md`](model-catalog-schema.md)：Provider/LogicalModel/Deployment 分层、能力/区域契约与绑定漂移防护。
- [`model-authorization-and-listing.md`](model-authorization-and-listing.md)：项目/Key 白名单交集、目录可用性与安全 `/v1/models`。
