# P10-T04 用量事件发布验收报告

- 日期：2026-08-04
- 范围：Attempt 终态 UsageEvent、事务化 Outbox、有限批次 Kafka Relay、失败重试与租约恢复
- 结论：实现、本机完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

实现提交 `46595de` 新增 `UsageEvent` version 1 Wire Contract 和 Migration 16。`execution.PostgresRecorder` 在 `CompleteAttempt`、`CompleteAttemptForRetry` 的既有 PostgreSQL 事务中，把 Attempt/Request 终态与逐 Token 类型 Outbox 事实一起提交；任何 Outbox 唯一约束或写入错误都会回滚终态，网关请求线程不执行 Kafka 或 ClickHouse I/O。

Normalized Usage 的缺失或显式零值不生成可计费事件。Gateway 仅能生成 `provider → usage.observed` 和 `estimated → usage.estimated`；reconciled/adjustment 保留给后续受信任流程。当前 Chat Adapter 的 input、output、cache read/write、reasoning、audio input/output 七个维度可独立发布，图像维度保留在有限领域与 Schema 契约中。

## 2. Relay 与交付边界

Gateway 后台 Relay 以有限批次和 `FOR UPDATE SKIP LOCKED` 认领 `pending` 事件，短事务结束后再等待 Kafka ACK。发布成功推进为 `published`；失败返回 `pending`，只保存 `EVENT_BUS_UNAVAILABLE` 等安全错误码并按 1 秒至 1 分钟指数退避。进程退出遗留的 `publishing` 租约到期后会被重领，多个实例可并行工作而不长期占用数据库事务。

Producer 使用 franz-go 的幂等写、全部 ISR ACK 和最多 1000 条客户端缓冲，固定发布到预创建的 `ai-gateway.usage.v1`，事件 ID 同时作为 Kafka Key。Kafka ACK 与 PostgreSQL 标记之间仍可能在进程故障时重复发送，因此公开语义明确为至少一次；P10-T05 消费者必须按稳定 eventId 幂等落账，不能宣称跨系统恰好一次。

## 3. 安全与数据约束

Wire Payload 只含事件、Tenant、Request、Attempt、Deployment、Token 类型、正整数数量、来源、完整性、观察时间和 Trace/Span 身份，不含 Prompt、Response、Credential、Provider Secret、Endpoint、Raw Evidence 或 Broker 私密错误。

数据库以复合外键固定 Request/Attempt/Deployment 可信归属，以 `(request_id, attempt_id, token_type, source)` 防止同一终态维度重复，并由触发器锁定事实字段。`pending`/`publishing` 行不能删除，只有已发布行允许未来保留任务清理。

## 4. 本机门禁

- `go test -count=20 -cover ./internal/metering ./internal/meteringoutbox`：连续 20 轮通过，领域契约覆盖率 98.9%，Relay 数据库主路径由真实集成测试覆盖；
- 真实 PostgreSQL/Redpanda 两个 P10-T04 专项连续 20 轮通过，包括失败持久重试、事件恢复发布、租约过期重领和真实 Broker ACK；
- `scripts/dev.ps1 -Action test-integration`：完整 PostgreSQL、Redis、Redpanda 与进程集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测/构建、govulncheck、16 个迁移顺序、Actions 语法与本地密钥扫描全部通过；
- 经单独授权，本机空 Outbox 执行 `16→15→16` 成功，两端均为 `dirty=false`；恢复后的 Gateway Execution、Usage Ledger、Taxonomy、PriceVersion、Outbox 和 Kafka ACK 回归全部通过；
- Compose Topic 初始化与 CI 修复后的独立临时 Redpanda 均确认 `ai-gateway.usage.v1` 为 6 分区、1 副本。

## 5. 远端证据

首轮 Actions `30881054100` 暴露 CI 容器内 `rpk` 无法解析 `redpanda:9092`，业务代码、go-quality 和安全 Job 未失败。提交 `a587f25` 将 CI 专用内部广播地址修为容器内可解析的 `localhost:9092`，本地独立容器复现成功后重新推送。

GitHub Actions [`30881305838`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30881305838) 最终三个 Job 全绿：`go-quality` 通过 Linux race、lint、构建与漏洞门禁；`migration-integration` 通过 Redpanda Topic 创建、真实 Kafka ACK、PostgreSQL/Redis 集成和 Migration `16→15→16`；`config-and-secrets` 通过 YAML 与双重密钥扫描。
