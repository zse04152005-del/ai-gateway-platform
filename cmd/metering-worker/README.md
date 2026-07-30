# metering-worker

异步计量进程。后续负责消费至少一次投递的用量事件、幂等写入 Usage Ledger、结算预算并异步写分析存储；当前 P03-T03 建立事件总线 bootstrap 连接、停止信号响应与会话关闭生命周期。

## 启动

从项目根目录执行：

```powershell
Copy-Item .env.example .env  # 仅首次需要
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action metering-worker
```

worker 按 `KAFKA_BROKERS` 顺序尝试 broker，首个 TCP bootstrap 连接成功后进入等待状态。收到 `Ctrl+C`、Windows interrupt 或 `SIGTERM` 后停止 connected 状态，并在 `SHUTDOWN_TIMEOUT` 内关闭事件总线会话。

## 当前边界

- 已实现：broker 地址强校验、顺序回退、连接超时、连接状态、取消启动、有限时关闭与错误传播。
- 尚未实现：Kafka metadata/consumer group、Usage Event 反序列化、幂等账本和 offset 提交；这些属于 P09/P10。
- 当前 TCP bootstrap 只证明网络与监听端点可连接，不伪装为 Kafka 协议消费已经完成。

真实 Redpanda 连接测试：

```powershell
$env:KAFKA_BROKERS='localhost:19092'
go test -count=1 -tags=integration -run TestMeteringWorkerConnectsToEventBus ./tests/integration
```
