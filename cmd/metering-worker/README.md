# metering-worker

异步计量进程。当前使用 Kafka Consumer Group 消费至少一次投递的 UsageEvent，以 PostgreSQL Receipt 幂等写入带价格版本的 Usage Ledger；后续继续负责多 Attempt 聚合、预算结算与分析存储。

## 启动

从项目根目录执行：

```powershell
Copy-Item .env.example .env  # 仅首次需要
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action metering-worker
```

worker 使用固定消费者组 `ai-gateway-metering-v1` 订阅预创建的 `ai-gateway.usage.v1`。它禁用自动 offset 提交：每条事件必须先完成 Receipt、价格选择和 Ledger 的 PostgreSQL 事务，再同步提交 offset。收到 `Ctrl+C`、Windows interrupt 或 `SIGTERM` 后停止 Poll，并在 `SHUTDOWN_TIMEOUT` 内关闭消费者会话。

## 当前边界

- 已实现：Kafka metadata/consumer group、有限 Poll、再均衡阻塞、严格 UsageEvent 解码、Receipt/Ledger 事务幂等、有效价格选择、整数 micros 计算、手动 offset 提交和有限时关闭。
- 失败策略：数据库/价格/Payload/offset 任一步失败均不确认当前事件，进程返回安全分类错误，由编排器重启后重放。
- 尚未实现：DLQ/积压告警和人工恢复 Runbook（P13）、多 Attempt 聚合（P10-T06）、Adjustment（P10-T08）与 ClickHouse 分析写入（P11）。

真实 PostgreSQL/Redpanda 幂等测试：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action test-integration
```
