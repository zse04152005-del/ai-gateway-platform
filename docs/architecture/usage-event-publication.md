# 用量事件事务化发布

P10-T04 把每个 RouteAttempt 的终态 Usage 转换为内容无关、可独立定价的 UsageEvent，并通过 PostgreSQL Transactional Outbox 异步发布到 `ai-gateway.usage.v1`。请求热路径只等待原有终态事务，不等待 Kafka、ClickHouse 或后续计量消费者。

## 事实与事务边界

`execution.PostgresRecorder` 在 `CompleteAttempt` 和 `CompleteAttemptForRetry` 中完成同一事务：

1. 校验并推进 RouteAttempt 终态；
2. 在最终请求路径推进 GatewayRequest 终态；
3. 将 Normalized Usage 拆为逐 Token 类型的正数量事件；
4. 用一次 `unnest` 批量插入 `app.usage_event_outbox`；
5. 一起提交或一起回滚。

因此 Provider 已产生费用的 Attempt 不会出现“终态已提交但发布事实丢失”的窗口。相同 Request、Attempt、Token 类型与来源只能存在一条 Outbox 事实；唯一约束冲突会使整个终态事务回滚，而不是覆盖旧事件。

缺失和显式零值不产生可计费事件。Gateway 只允许 Provider Usage 映射为 `usage.observed/provider`，本地估算映射为 `usage.estimated/estimated`；`reconciled` 和 `adjustment` 必须由后续受信任流程生成，Gateway 不能冒充。P10-T07 起新事件使用 Schema v2：estimated 事件必须携带 `estimated=true`、tokenizer/version、physical model、Deployment version 和 provider protocol version，Provider 事件禁止携带这些字段。Consumer 在同一 `ai-gateway.usage.v1` Topic 上继续接受无估算元数据的历史 Schema v1，避免升级窗口遗留事件失效。当前 Chat Usage 可发布 input、output、cache read/write、reasoning、audio input/output 七个维度；图像维度保留在领域与 Schema 契约中，等 Adapter 提供对应事实后再启用。

## 异步 Relay

Gateway 内的后台 Relay 每次最多认领有限批次，以 `FOR UPDATE SKIP LOCKED` 支持多实例并行：

```text
pending --claim/lease--> publishing --broker ACK--> published
                              |
                              +--failure/timeout--> pending + exponential backoff
                              +--process loss------> lease expiry + reclaim
```

数据库事务只覆盖认领或状态推进，Kafka I/O 在事务外执行。发布失败保存固定安全错误码，不持久化 Broker 地址、认证材料或原始错误。退避从 1 秒指数增长到最多 1 分钟；默认批次 100、发布超时 2 秒、租约 10 秒、轮询间隔 250ms，所有边界均有限。

Kafka ACK 与 PostgreSQL `published` 标记之间不能形成跨系统原子提交：若 ACK 已成功但状态更新前进程退出，租约到期后会重复发布。Relay 保持原 `event_id`，并把它作为 Kafka Key；P10-T05 消费者必须用该 ID 幂等落账。因此交付语义是至少一次，而不是虚假的恰好一次。

## Wire Contract 与安全边界

UsageEvent `schema_version=1`，只包含事件、Tenant、Request、Attempt、Deployment、Token 类型、明确计费单位、整数数量、来源、完整性、观察时间和 Trace/Span 身份。当前 Gateway 的 Normalized Usage 均为 Token Count，因此显式发布 `billing_unit=token`；消费者对字段加入前的 version-1 消息也只允许向后兼容为 token。它不包含 Prompt、Response、Virtual Credential、Provider Secret、Endpoint、Raw Usage Evidence 或 Broker 私密错误。

Producer 使用 franz-go 幂等 Producer、全部 ISR ACK 和最多 1000 条客户端缓冲。Topic 必须由部署流程预创建为 `ai-gateway.usage.v1`；Compose 和 CI 当前创建 6 分区、单节点环境 1 副本，不依赖 Broker 自动建 Topic。生产环境应按可用性目标提高副本数和 `min.insync.replicas`。

## 数据保留与恢复

Outbox 业务字段一经写入不可修改；只有租约、重试和发布状态可以按状态机推进。`pending` 或 `publishing` 行不能删除，`published` 行可由未来保留任务清理。Relay 故障不终止 Gateway，也不改变已提交 Usage 事实；恢复 Broker 或数据库后会继续认领。Migration 16 Down 会删除 Outbox 全部事件与发布状态，只允许在明确获批的开发回滚验证中执行。
