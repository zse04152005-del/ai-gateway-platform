# protocolcanary

`protocolcanary` 通过 Provider Adapter 的公开接口执行低成本、内容安全的协议漂移探针。周期调度器只依赖 `Executor.Run(ctx, Probe)`；本包不自行创建后台 Goroutine，也不决定生产调度频率。

安全边界：

- Probe 只允许一条 1～256 字节的合成 User Text；
- `MaxOutputTokens` 强制为 1～16；
- 禁止 Tool、Structured Output 和 Policy Content；
- 单次 Timeout 为 10 ms～30 s；
- Stream 最多读取 1～4096 个 Chunk；
- Result 不包含 Prompt、Response、Raw Body、Provider Message、Tool Arguments、Credential 或内部 Error；
- 未知 Finish Reason 与 Provider Extension 只保存 SHA-256 指纹；
- 未映射 Usage 只保存经过 Adapter 校验的 JSON Pointer。

结果区分 `stable`、`drift`、`provider_failure`、`transport_failure`、`timeout` 和 `cancelled`。协议解析失败必须实现 `provideradapter.ProtocolViolation`，只公开规范化 Operation/Code；Mock Adapter 的 `ProtocolError` 已实现该接口。

Mock 验证使用真实共享 HTTP Handler 和 `httptest.Server`，覆盖普通/SSE 稳定、未知结构、Usage/Finish 漂移、Provider Extension、缺失 Usage、Chunk 上限、429、Timeout 和 Caller Cancellation。完整契约见 `docs/api/protocol-canary.md`。
