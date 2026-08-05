# 多 Attempt 成本聚合

> 状态：Implemented
>
> 日期：2026-08-05
>
> 对应任务：P10-T06

## 1. 权威事实与聚合选择

请求费用不覆盖写入 `gateway_requests.cost`，也不维护第二份可漂移的可变总额。`internal/meteringcost` 在查询时从不可变 `usage_ledger_entries` 重建 Request 总额和每个 RouteAttempt 的费用；因此幂等重放不会重复计入，后续 P10-T08 Adjustment 也能沿同一追加式事实链进入聚合。

一次聚合返回 Request 的全部物理 Attempt，按 `attempt_no` 排序，包括没有正数量 Usage 的零费用 Attempt。`retryable_failed`、`failed`、`partial_failed`、`cancelled` 与 `succeeded` 都是费用事实，不按“最终成功”过滤。没有物理 Attempt 的缓存等 Request 级分录保留在独立 bucket，不能伪装成 Provider Attempt。

## 2. 完整性屏障

聚合在 PostgreSQL `REPEATABLE READ READ ONLY` 快照内依次确认：

1. Request 位于可信 Tenant+Project Scope 且已经终态；
2. `attempt_count` 与实际 RouteAttempt 数量一致，每个 Attempt 也已终态且序号连续；
3. 该 Request 的每一条事务化 Usage Event Outbox 事实都已存在 Tenant/Request/Attempt 一致的 Ledger 行；
4. 才读取价格版本币种与整数 `amount_micros` 并构建结果。

只要有一条 Outbox 事实尚未被异步消费者定价，结果就是 `ErrPending`，不会把暂时缺失的费用显示为 0。活动 Request 返回 `ErrNotTerminal`；数据库事实损坏、数量漂移或聚合溢出 fail closed 为 `ErrUnavailable`。

## 3. 币种与精度

每个 Ledger 行已经锁定 PriceVersion。不同 Deployment 可能合法使用不同币种，因此 Request 和 Attempt 都以 ISO 三位大写币种分别汇总，禁止把 USD 与 CNY 机械相加。单行和每个币种聚合总额都必须在 `0..2^53-1`，保证后续 JSON 控制面交换仍保持整数精确性。

当前 `amount_micros` 非负；P10-T08 引入 Adjustment 时必须继续使用追加分录并显式扩展带符号金额约束，不能用 UPDATE 改写本模块已经读取的原始费用。

## 4. 部分流边界

根据首模型 Token 不可逆规则，`partial_failed` Request 不会再透明启动一个最终成功 Attempt。因此验收包含两个独立事实场景：首包前的 retryable-failed Attempt 加最终成功 Attempt 必须相加；首包后的 partial-failed Attempt 必须独立保留并计费。普通 failed Attempt 即使客户端未收到有效输出，只要 Provider Usage 已知也同样计入。

## 5. 安全与验证

读取必须同时携带可信 Tenant 和 Project；不存在、跨租户与跨项目统一为 `ErrNotFound`。返回内容只有执行身份、有限状态、Ledger 行数、币种和整数金额，不读取 Prompt、Response、Credential、Provider Secret、Endpoint 或 Raw Usage Evidence。公开 HTTP 费用查询在 P10-T09 单独接入授权与响应契约。

真实 PostgreSQL 测试验证异步缺口 pending、跨 Scope 隐藏、两 Attempt 失败加成功总额、partial-failed 与 failed 费用，以及每个 Attempt/Request 总额完全一致。纯领域测试补充零费用 Attempt、多币种隔离、Request 级 bucket、重复事件、未知 Attempt、状态漂移与总额溢出。
