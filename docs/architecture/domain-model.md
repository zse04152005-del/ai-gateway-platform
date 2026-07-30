# 核心领域模型与术语

> 状态：Accepted for MVP  
> 日期：2026-07-30  
> 对应任务：P01-T06

## 1. 领域边界

| 领域 | 主要实体 | 事实源 |
|---|---|---|
| Identity & Tenancy | Tenant、Project、VirtualKey | PostgreSQL |
| Model Catalog | Provider、LogicalModel、Deployment、Capability | PostgreSQL + Snapshot |
| Routing | RoutePolicy、RouteDecision、HealthState | 配置 PostgreSQL；健康为运行态 |
| Gateway Execution | GatewayRequest、RouteAttempt、StreamSegment | PostgreSQL/可恢复写入 |
| Limits & Budget | LimitPolicy、BudgetAccount、BudgetReservation | PostgreSQL；Redis 快速保护 |
| Metering | UsageEvent、UsageLedger、PriceVersion、Adjustment | PostgreSQL；ClickHouse 副本 |
| Governance | AuditEvent、ConfigVersion、SecretReference | PostgreSQL/密钥系统 |

## 2. 术语定义

### Tenant

企业或隔离组织边界。所有项目、Key、策略、请求、用量和缓存必须归属一个 Tenant。

### Project

Tenant 内的应用或成本归集边界。预算、模型白名单和虚拟 Key 通常绑定 Project。

### VirtualKey

应用访问网关的凭证。只保存哈希与可识别前缀，不等于外部 Provider Key。

### Provider

模型服务供应商或自托管平台类型，例如 Mock、某云模型平台或 vLLM 集群。

### LogicalModel

客户端看到的稳定模型名称，例如 `general-chat`。它描述业务能力而非单个物理端点。

### Deployment

可被实际调用的物理模型端点，绑定 Provider、真实模型名、区域、能力、密钥引用和价格。

### RoutePolicy

不可变发布版本，描述候选过滤、优先级、权重、回退和超时。草稿不参与数据面执行。

### GatewayRequest

一次客户端调用。它可以拥有一个或多个 RouteAttempt；状态不等于最终计费状态。

### RouteAttempt

一次对具体 Deployment 的上游尝试。每次重试/故障切换都创建新 Attempt，独立记录状态、TTFT、结束原因和 Usage。

### StreamSegment

流式 Attempt 中可核算的输出片段/阶段。MVP 不要求保存内容，只保存序号、字节、Token 估算、时间与结束原因。

### UsageEvent

从数据面发往计量链路的幂等事件，描述一个 Attempt 的用量事实或状态变化。

### UsageLedger

不可变计量分录。不同 Token 类型和费用分别记录；修正通过 Adjustment，不覆盖历史。

### BudgetReservation

请求执行前为最坏可接受费用创建的临时占用，完成后结算实际费用并释放差额。

## 3. 聚合与一致性边界

### Identity 聚合

- Tenant 是根；Project 与 VirtualKey 不能跨 Tenant。
- Key 轮换生成新密钥材料，不覆盖审计历史。

### Model Catalog 聚合

- LogicalModel 绑定一个或多个 Deployment。
- Deployment 的能力、区域、密钥和状态必须通过发布校验。
- 逻辑模型名称是租户内稳定 API 契约；Provider 物理模型名和 Endpoint 不向客户端暴露。
- Binding 创建以及 LogicalModel/Deployment 能力更新都必须保持能力、Token 上限、数据保留模式和区域兼容。
- 未知 Capability 字段必须拒绝，不能静默忽略后让路由误判能力。
- Deployment 只保存同 Provider 的 Secret Reference ID；本地开发为认证加密 Envelope，生产只保存 Vault/KMS Locator。
- 价格是独立版本，不能直接覆盖历史生效记录。

### Gateway Execution 聚合

- GatewayRequest 创建后可以追加 RouteAttempt。
- Attempt 状态迁移有条件更新，旧 Worker/旧协程不能覆盖最终状态。
- Request 的最终费用由 Ledger 汇总，而不是直接覆盖字段。

### Budget 聚合

- Account 以 tenant/project/key/session 等 scope 标识。
- Reservation 原子占用可用余额。
- Settlement 引用 Request 和 Ledger，具有幂等键。

## 4. 状态机

### GatewayRequest

```text
RECEIVED -> AUTHORIZED -> RESERVED -> ROUTING -> RUNNING
RUNNING -> SUCCEEDED
RUNNING -> PARTIAL_FAILED
RUNNING -> FAILED
RUNNING -> CANCELLED
```

- `PARTIAL_FAILED`：客户端已收到内容但请求未正常完成。
- `FAILED`：没有向客户端交付有效模型输出。
- `CANCELLED`：客户端取消或平台明确取消；Provider 费用仍由 Attempt Ledger 决定。

### RouteAttempt

```text
CREATED -> CONNECTING -> HEADERS_RECEIVED -> STREAMING -> SUCCEEDED
CONNECTING/HEADERS_RECEIVED -> RETRYABLE_FAILED
CONNECTING/HEADERS_RECEIVED/STREAMING -> FAILED
STREAMING -> PARTIAL_FAILED
any active -> CANCELLED
```

只有首个客户端可见模型内容之前的 `RETRYABLE_FAILED` 可以触发备用 Deployment。

### BudgetReservation

```text
PENDING -> ACTIVE -> SETTLED
ACTIVE -> RELEASED
ACTIVE -> EXPIRED -> RECONCILIATION_REQUIRED
```

## 5. 不变量

1. 所有租户资源必须有 tenantId。
2. Request、Attempt 和 Ledger 一旦归属 Tenant/Project 不可更换。
3. 每个 Attempt 的序号在 Request 内唯一。
4. 每个 UsageEvent eventId 全局唯一，重复消费不产生第二份有效分录。
5. Ledger 只追加；Adjustment 引用被修正分录。
6. 已发布 RoutePolicy、ConfigVersion 和 PriceVersion 不可原地修改。
7. 首包后不透明跨 Deployment 拼接模型输出。
8. 没有成功预算预留时，不执行受硬预算约束的上游调用。

## 6. 领域事件

- `gateway.request.received`
- `gateway.request.completed`
- `gateway.attempt.started`
- `gateway.attempt.first_byte`
- `gateway.attempt.completed`
- `gateway.attempt.partial_failed`
- `usage.observed`
- `usage.estimated`
- `budget.reserved`
- `budget.settled`
- `budget.reservation_expired`
- `config.published`
- `virtual_key.rotated`

事件不是跨服务共享数据库实体的替代品；每个消费者必须幂等并支持 Schema 版本。
