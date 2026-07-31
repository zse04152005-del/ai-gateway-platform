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
- [`provider-secret-references.md`](provider-secret-references.md)：Provider 绑定引用、本地 AES-GCM 信封与 Vault/KMS Resolver 边界。
- [`minimum-deployment-selection.md`](minimum-deployment-selection.md)：可信作用域候选、请求能力过滤与最小优先级选择。
- [`upstream-http-client.md`](upstream-http-client.md)：进程级 Provider Client、分层超时、连接池和出站 Header 信任边界。
- [`non-stream-execution.md`](non-stream-execution.md)：Selection→Adapter→HTTP→Normalized Response 的单 Attempt 边界与错误隔离。
- [`active-health-checks.md`](active-health-checks.md)：冷路由低成本探针、连接池/计费隔离、抖动调度与迟滞恢复。
- [`circuit-breaker.md`](circuit-breaker.md)：Closed/Open/Half-Open、Generation Permit、并发探测和错误归因。
- [`retry-classification.md`](retry-classification.md)：错误有限分类、首输出不可逆边界、请求级 Attempt/时间/费用预算和重复计费风险。
- [`failover-orchestration.md`](failover-orchestration.md)：首次客户端输出前的有界重选、独立 Attempt、父 Request 连续性和多 Attempt 费用事实。
- [`route-decision-records.md`](route-decision-records.md)：候选过滤、路由评分、重试因果链和最终选择的无内容持久化复盘。
- [`limit-policy-schema.md`](limit-policy-schema.md)：RPM/TPM/并发 soft/hard 边界、逐字段继承、强类型租户引用和兼容迁移。
- [`local-fast-limiter.md`](local-fast-limiter.md)：Platform/Tenant/Project/Key 四层原子 admission、分钟窗口、并发 Lease 和配置热更新。
- [`redis-rpm-limiter.md`](redis-rpm-limiter.md)：Redis TIME 权威分钟、四层单 Hash Lua 原子计数、绝对 TTL 和时钟边界重试。
- [`tpm-reservation-settlement.md`](tpm-reservation-settlement.md)：版本化输入估算、输入加最大输出预留、原分钟幂等结算、差额释放与超额补记。
- [`gateway-execution-records.md`](gateway-execution-records.md)：GatewayRequest/RouteAttempt 事实边界、状态机、CAS、追加式证据与 fail-closed 调用顺序。
- [`stream-cancellation.md`](stream-cancellation.md)：客户端取消到 Adapter/Body/Provider Context 的传播、释放证据和泄漏回归边界。
