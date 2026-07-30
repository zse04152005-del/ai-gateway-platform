# 0004 Outbox 与不可变 Usage Ledger

- 状态：Accepted
- 日期：2026-07-30

## 背景

模型调用可能包含多个 Attempt、部分流、缓存 Token 和供应商后续账单修正。把总费用直接覆盖在请求记录上会丢失证据，也难以幂等和对账。

## 决策

- Request/Attempt 状态与待发布用量事件使用事务/可恢复 Outbox 策略。
- Metering Worker 至少一次消费，以 eventId 唯一约束保证幂等。
- Usage Ledger 只追加；修正使用 Adjustment 分录引用原记录。
- 请求总费用为所有有效 Ledger 分录之和。

## 替代方案

- 同步写 ClickHouse：分析故障会阻断调用。
- 直接更新 request.cost：简单但无法解释重试、部分失败和修正。
- 依赖消息队列 exactly-once：跨数据库和外部系统无法提供完整保证。

## 后果

- 可以完整对账和审计。
- 查询需要聚合或维护可重建摘要。
- Outbox 与 Reconciliation Worker 增加实现复杂度。

## 验证方式

P10 重复事件、多 Attempt、部分流、缓存 Token 与 Adjustment 测试。

