# P10-T05 Metering 幂等消费验收报告

- 日期：2026-08-05
- 范围：Kafka 消费者组、UsageEvent 校验、Receipt 幂等屏障、可信定价与 Usage Ledger 原子落账
- 结论：实现、本机完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

实现提交 `4e5ffe4` 将 `metering-worker` 接入 franz-go 消费者组，关闭自动提交，并限制每次 Poll 最多 100 条记录。每条记录只有在 PostgreSQL 事务完成后才同步提交 Offset；取消和关闭流程有界，暂时性 Broker 不可用由客户端重连处理。交付语义仍是 Kafka 至少一次加数据库幂等，不宣称跨系统恰好一次。

消费者只接受 64 KiB 以内、严格符合版本契约的 UsageEvent，并以规范 JSON 的 SHA-256 保存语义指纹。Migration 17 新增不可变 Usage Event Receipt；Receipt 与 Usage Ledger 在同一事务写入。同一 consumer group 与 eventId 的相同事实重放直接返回 replay，不产生第二份 Ledger；相同 eventId 携带不同事实会被判定为冲突且不提交 Offset。

## 2. 定价与计量边界

事务按事件的 Deployment、Region、Currency、Token 类型、billing unit 和 observedAt 选择已经发布且生效的 PriceVersion/PriceRate，再把价格版本和整数 micros 金额锁定在 Ledger。金额使用任意精度中间值并向上取整，避免浮点误差或整数溢出造成少计费。

Migration 18 为 UsageEvent 与 Outbox 明确增加 `billing_unit=token`；旧 version 1 事件缺失该字段时仅兼容归一为 token。PriceRate 单位必须与事件单位严格相同，因此 audio token 不会误套按 second 定价的费率。当前 Gateway 只发布 Token 数量事实；second/image 计量需要未来显式升级事件契约。

## 3. 失败、安全与恢复语义

无效事件、冲突事件或找不到可信费率时，消费者停止并保留未提交 Offset，避免错误事实被静默跳过。DLQ、告警和人工处置 Runbook 留在 P13 统一实现。错误对外只保留安全分类，不暴露 Kafka 地址、数据库 URL、Payload、Prompt、Response、Credential 或原始 Provider Evidence。

Receipt 的语义指纹是幂等屏障；Migration Down 会删除该屏障，即使已有 Ledger 仍保留，也不得把生产回滚描述为无损操作。本次只在确认本机 Receipt 表与 Outbox 新字段均为空后执行授权的恢复演练。

## 4. 本机门禁

- 真实 PostgreSQL/Redpanda 专项验证同一 eventId 发布 10 次得到 1 次 insert、9 次 replay、1 条 Receipt 和 1 条 Ledger，10 个 Offset 均在对应数据库事务后提交；
- 数量 13、等价每 token 2.5 micros 的费率得到向上取整后的 33 micros；同 ID 不同事实被拒绝，audio token 不能匹配 second 费率；
- P10-T05 真实专项连续 20 轮通过；`internal/metering` 覆盖率 99.0%，`internal/meteringworker` 覆盖率 83.8%，消费者数据库/Kafka 主路径由真实集成测试覆盖；
- 完整 integration 和 `scripts/dev.ps1 -Action check` 通过，包括 race、lint、构建、漏洞、迁移顺序、YAML 与密钥扫描；
- 经单独授权，本机空表执行 Migration `18→16→18` 成功，两个端点均为 `dirty=false`；Metering Consumer、Usage Outbox/Kafka ACK、Usage Ledger/Taxonomy/PriceVersion 与 Gateway Execution 回归均通过。

## 5. 远端证据

首轮 Actions `30967899428` 的 `migration-integration` 与 `config-and-secrets` 通过；Linux 生命周期测试读取到关闭前已排队的 Kafka 握手字节，旧探针将其误判为连接仍打开。测试修复提交 `043beeb` 改为排空已排队字节，并以读超时判定连接未关闭；EOF 或连接重置才视为正确释放。

GitHub Actions [`30968113656`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30968113656) 最终三个 Job 全绿：`go-quality` 通过 Linux race、进程生命周期、lint、构建与漏洞门禁；`migration-integration` 通过真实 PostgreSQL/Redis/Redpanda 集成和 Migration `18→16→18`；`config-and-secrets` 通过 YAML 与双重密钥扫描。
