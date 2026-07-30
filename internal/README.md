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
- `limits`：RPM/TPM/并发。
- `budget`：Account、Reservation 和 Settlement。
- `metering`：Usage Event、Ledger 和价格。
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
- `meteringworker`：计量进程的事件总线 bootstrap 会话与强制有限时停止生命周期；不提前承载消费/账本规则。
