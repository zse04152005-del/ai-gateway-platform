# P07-T10 流式压力与泄漏验证报告

- 日期：2026-07-31
- 范围：真实 TCP/HTTP SSE、共享上游连接池、Mock Adapter、TimeoutController、Buffer、FailoverGate、UsageAggregator
- 主测试：`TestStreamingMixedPressureReleasesConnectionsGoroutinesAndBuffers`

## 1. 负载模型

每轮并发运行 50 个独立流式会话，共 3 批 150 个；五种场景各 30 个：

1. 正常流：MessageStart、4 个模型 Chunk、完整 Provider Usage 与 `[DONE]`；
2. 慢客户端：MessageStart、64 个模型 Chunk、完整 Usage，消费者每个事件停顿 5 ms；
3. 随机取消：固定种子 `20260731`，首个模型 Chunk 后随机等待 1～15 ms 取消；
4. 上游断流：首个模型 Chunk 后直接关闭连接，不发送终态和 `[DONE]`；
5. 长 TTFT：只发送 MessageStart 与 Provider heartbeat，超过 50 ms 首 Token SLO。

每个会话通过生产级 `upstreamhttp.Client` 的共享 Transport/连接池建立真实连接，使用 Mock Adapter 的真实 SSE Decoder 解析，再经有界 Buffer 交给慢/快消费者。客户端提交路径由 FailoverGate 保护，UsageAggregator 同时验证从 0 开始的真实 Adapter Sequence 和 Provider/Estimate 回退。

## 2. 强制不变量

- 单会话最多 4 个排队 Chunk、16 KiB 保守内存预算；高水位不得越界；
- 背压最多等待 250 ms，慢客户端必须产生可观测等待且不能溢出；
- 每会话最多 5 秒，测试批次最多 8 秒；
- 正常/慢流必须以精确 `io.EOF` 结束并选择 `provider_final` Usage；
- 包装底层 `io.EOF` 的 `unexpected_eof` 必须保持 Protocol Error，不能误判为正常结束；
- 随机取消必须在首包后传播到上游，记录 CancellationObservedAt/UpstreamReleasedAt；
- 长 TTFT 必须分类为 first-token timeout、failover eligible、非 partial；
- 上游断流必须保留已输出模型事实并回退 local estimate；
- 每个会话只启动一个物理 Attempt，所有 Gate 最终关闭；
- 每批结束 active Handler 为 0；全测结束连接在 3 秒内归零；
- GC 后 Goroutine 不超过基线 +12，保留堆增长不超过 32 MiB。

## 3. 实测结果

工作副本首次成功样本：

```text
sessions=150
duration=1.1090931s
peak_handlers=21
max_buffer_chunks=4
max_buffer_bytes=2480
backpressure_waits=1836
goroutines=3->2
heap=1096688->2158600
```

随后执行 10 轮稳定性回归，共 1500 个真实流式会话、每种场景 300 次，全部通过：

```powershell
go test ./internal/streaming -run TestStreamingMixedPressure -count=10 -timeout=3m
```

结果：`ok`，总用时约 11.1 秒。数值是本次开发机证据而非生产容量承诺；容量基线与持续长连接测试在 P18 使用 k6/Vegeta 和部署级 CPU/内存/FD 指标重新测量。

## 4. 缺陷发现

1. P07-T09 初版把未初始化 `lastSequence=0` 与合法首 Chunk Sequence=0 混淆。已在提交 `75c4c5a` 中用独立 `SequenceObserved` 修复，并重新通过完整门禁与 CI。
2. 压测首轮把 `errors.Is(protocolError, io.EOF)` 当成正常结束；Provider `unexpected_eof` 正确包裹底层 EOF，因此这种判断会吞掉断流。组合管线已改为只有精确 `err == io.EOF` 才执行正常 Finish，包装 EOF 继续作为部分失败传播。

## 5. 复验命令

```powershell
go test ./internal/streaming -run TestStreamingMixedPressure -count=10 -timeout=3m
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action check
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action test-integration
```

GitHub Actions 的 Linux `go test -race -count=1 ./...` 是最终并发竞态门禁；本地 Windows 环境没有 GCC，不能把未执行的本机 race 声称为证据。
