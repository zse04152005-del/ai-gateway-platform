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
- `meteringworker`：计量进程的事件总线 bootstrap 会话与强制有限时停止生命周期；不提前承载消费/账本规则。
