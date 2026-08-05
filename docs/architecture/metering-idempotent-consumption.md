# Metering 幂等消费

P10-T05 由 `metering-worker` 使用固定消费者组 `ai-gateway-metering-v1` 消费 `ai-gateway.usage.v1`。Kafka 负责至少一次投递，PostgreSQL 的不可变 Receipt 与 Usage Ledger 事务负责业务幂等；系统不依赖 Broker 的“恰好一次”宣传来保证不会重复计费。

## 消费与 Offset 边界

franz-go Consumer 禁用自动提交，每次最多 Poll 100 条，单 Broker/Partition Fetch 上限为 1 MiB，单条 UsageEvent 在应用层限制为 64 KiB。`BlockRebalanceOnPoll` 保证处理和同步提交期间 Partition 不被悄悄转交；每条记录只有在 PostgreSQL 事务提交成功后才同步提交 offset，然后才允许再均衡。

```text
Kafka record
  -> strict decode + canonical SHA-256
  -> PostgreSQL transaction
       -> insert immutable Receipt(eventId, fingerprint)
       -> select trusted effective PriceRate
       -> integer amount calculation
       -> insert append-only Usage Ledger
  -> transaction commit
  -> Kafka offset commit
```

数据库、价格、Payload 或 offset 提交失败都会使进程返回安全分类错误，当前记录不会被确认。编排器重启后会从未提交 offset 重试。无效事件不会被静默跳过；DLQ、告警和人工恢复 Runbook 在 P13 上线门禁前补齐。

P10-T06 的 Request 聚合还会比较 Outbox 与 Ledger：只要本 Request 有一个已持久化事件尚未形成 Ledger，就返回 pending 而不是暂时少计费。完整边界见 [`multi-attempt-cost-aggregation.md`](multi-attempt-cost-aggregation.md)。

## Receipt 幂等事务

Migration 17 创建 `app.usage_event_receipts`。Receipt 以 eventId 为主键，保存通过严格校验后规范化 JSON 的 SHA-256、Schema 版本、消费者组和消费时间，并通过延迟外键绑定同一 eventId 的 Usage Ledger。Receipt 和 Ledger 在同一事务中一起提交或一起回滚。

- 第一次事件赢得 Receipt 主键，选择价格并写入一条 Ledger；
- 同一 eventId、同一规范化事实的重放读取原 Receipt/Ledger并成功返回，不再次写入；
- 同一 eventId 被不同数量、归属或其他事实复用时，Fingerprint 不同，消费者返回冲突且不提交 offset；
- 并发消费者由 PostgreSQL 唯一约束串行化，不存在“先检查后双写”的竞态窗口。

Receipt 和 Ledger 都拒绝 UPDATE/DELETE。修正不能覆盖原事实，必须由 P10-T08 追加 Adjustment。

## 可信价格与整数金额

消费者不信任 Kafka Payload 自报的租户和 Deployment：价格查询同时核对 GatewayRequest 的 Tenant、RouteAttempt 的 Request/Deployment 和 Catalog Deployment 的 Region。它只选择 `observed_at` 时已生效的最新 published PriceVersion，并要求 Token 类型与 `billing_unit` 都存在完全匹配的 PriceRate。

当前 Gateway 的 Normalized Usage 全部是 Token Count，因此 version-1 新事件显式携带 `billing_unit=token`；为兼容字段加入前已发布的 version-1 消息，缺失单位只按 token 解释。Migration 18 为 Outbox 回填并锁定该字段。这样 audio token 不会误用 audio second 费率，未来秒数或图像数量必须先扩展明确的事件单位契约。

金额计算为 `ceil(quantity × unit_price_micros / unit_quantity)`，全程使用任意精度中间值，最终必须落在 `0..2^53-1`。正费率的小数量事实不会因整数截断静默变成免费，溢出则 fail closed。

## 安全边界

Decoder 拒绝未知字段、尾随 JSON、事件 Key/ID 不一致、非法枚举和超限 Payload。Fingerprint 基于验证后的规范化事件，因此 JSON 空白差异不制造冲突。错误对外只暴露有限分类，不记录 Payload、Broker 地址、数据库连接串、Prompt、Response、Credential 或 Raw Provider Evidence。
