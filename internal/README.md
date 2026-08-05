# internal

内部模块必须遵守以下依赖方向：

```text
transport -> application -> domain
infrastructure -> application/domain ports
domain 不依赖 transport、数据库、Redis、Kafka 或供应商 SDK
```

计划模块：

- `identity`：租户、项目和虚拟 Key。
- `catalog`：Provider、LogicalModel、Deployment 和 Capability。
- `routing`：候选过滤、决策、健康、重试和熔断。
- `proxy`：普通与 SSE 代理、取消和背压。
- `sse`：Provider-neutral 有界 SSE framing；统一处理网络分片、多行 data、comment、`[DONE]`、非法字段与行/事件资源上限。
- `streaming`：上游 Chunk 到下游 Writer 的双上限 FIFO、有限背压等待、CancelCause 和无内容高水位统计；全链路 total/首模型 Token/no-progress 超时控制器精确区分 Header、Provider/Gateway heartbeat 与客户端可见模型 Delta，并固定首包前 failover、首包后 partial-failed 边界；`FailoverGate` 原子串行化备用 Attempt 启动与模型输出提交，禁止首包后跨模型拼接和旧 Attempt 迟到写入；流式 Usage 以 Provider 终态、Provider 累计 Chunk、本地估算三轨保留并拒绝 meter 回退/重复求和；可选 Gateway heartbeat 仅输出固定 SSE comment，平台控制频率、客户端只能 on/off；客户端取消同步关闭上游并记录无内容传播耗时/释放时间。
- `limitpolicy`：RPM/TPM/并发 soft/hard 策略、逐字段继承、有效值与来源解析；后续限流器只消费解析结果。
- `limits`：单实例四层原子 RPM/TPM/并发 admission、幂等并发 Lease 和版本化热更新；Redis RPM 使用服务器权威分钟、单 Hash Lua 的四层先检查后递增和绝对 TTL，Redis 并发由 P09-T05 继续叠加。
- `budget`：Account、Reservation 和 Settlement。
- `metering`：Usage Event、Ledger 和价格。
- `meteringoutbox`：Attempt 终态事务 Outbox 的有限批次 Kafka Relay、租约恢复和安全重试状态机。
- `meteringconsumer`：UsageEvent 严格解码、语义 Fingerprint、可信价格选择、精确金额和 Receipt/Ledger 幂等事务。
- `meteringcost`：从不可变 Ledger 重建 Request/Attempt 分币种费用，并以终态与 Outbox 完整性屏障拒绝暂时少计费。
- `config`：版本化 Snapshot。
- `security`：脱敏、密钥端口和 SSRF 防护。
- `apierror`：内部 cause 与公开 HTTP 错误定义的安全隔离、统一渲染和重试元数据。
- `correlation`：有界 Request ID 冲突窗口、W3C Trace Context 中间件和下游 HTTP 传播。
- `observability`：日志、指标和 Trace；当前 JSON 日志固定输出服务身份与关联字段，并在 Handler 边界递归脱敏敏感属性。
- `httpserver`：HTTP 健康接口、请求/流注册表、普通请求有限排空和流式 Context 取消；不承载领域规则。
- `controlplane`：控制面管理路由装配；领域规则通过后续 application/domain 模块提供。
- `virtualkey`：虚拟凭据签发、HMAC 摘要、生命周期领域规则与 PostgreSQL 事务 Store；不依赖 HTTP。
- `keyauth`：数据面 Bearer 解析、版本化摘要验证、状态决策、有界正缓存和可信 Principal Context。
- `gateway`：数据面路由装配；`/v1/*` 统一先经过 `keyauth`，业务 Handler 不解析身份 Header。
- `catalog`：Provider、租户 LogicalModel、物理 Deployment、区域/数据保留/Token Capability Contract 及纯领域兼容性判定；不保存凭据。
- `providersecret`：Provider 绑定的 Secret Reference、development-only AES-256-GCM Envelope、Vault/KMS Resolver 端口与安全解析生命周期。
- `mockprovider`：本地确定性 OpenAI-compatible Provider 协议模拟器；覆盖普通、SSE、Usage、Tool Call、延迟、错误与断流，不访问外部依赖。
- `adapter`：供应商无关的 Request/Response/Chunk/Error/Usage 领域协议；严格区分缺失与 0，保留未知 Usage 原始证据，并提供深拷贝与内容安全日志边界。
- `provideradapter`：显式编译期 Factory 注册表与 Adapter 运行时端口；在启动/发布阶段聚合拒绝未知 Type，并对 Factory nil、身份漂移、类型错配和敏感内部错误 fail closed。
- `mockadapter`：注册类型 `mock` 的本地真实 HTTP Adapter；完整转换普通/SSE/工具/Usage/错误 Fixture，保留未知计量证据并在取消时关闭上游 Body。
- `adapterconformance`：统一 Adapter 真实 HTTP 契约测试运行器；注册前拒绝缺失 Fixture，并固定验证普通/SSE/取消/错误/缓存/工具/Finish/未知字段语义。
- `protocolcanary`：最小合成请求的协议漂移 Runner 与周期调度端口；只保留结构 Finding/Hash，区分 Drift、Provider/Transport Failure、Timeout 和 Cancellation。
- `openaiadapter`：官方 OpenAI Chat Completions 真实适配器；HTTPS、Provider Secret 最短解析边界、普通/SSE/Usage/错误归一化与离线一致性 Fixture。
- `upstreamhttp`：进程级 Provider HTTP Client、TLS/连接/首部/总超时、连接池复用、禁止重定向与出站 Header 信任边界；普通与流式 Client 共享 Transport，流式入口不施加普通响应的固定 Body 总超时。
- `proxy`：一个已选 Deployment 对应一次 Adapter/HTTP/Parse Attempt；只返回 Normalized Response 或安全分类，不内置重试。
- `execution`：可信 GatewayRequest、独立 RouteAttempt、乐观版本状态迁移、追加式状态证据和无内容 Usage Summary 的 PostgreSQL 记录边界。
- `meteringoutbox`：从 PostgreSQL 不可变 UsageEvent Outbox 向预创建 Kafka Topic 的至少一次后台发布；不阻塞请求热路径，稳定事件 ID 留给消费者幂等。
- `meteringconsumer`：固定 Kafka Consumer Group 的手动 offset 提交与不可变 Receipt；只有 Ledger 事务成功后才确认事件。
- `meteringcost`：可信 Tenant/Project Scope 下的 repeatable-read 聚合读取；失败、部分流、取消与成功 Attempt 都保留。
- `meteringworker`：计量进程的事件总线 bootstrap 会话与强制有限时停止生命周期；不提前承载消费/账本规则。
