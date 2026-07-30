# mockadapter

`mockadapter` 是注册类型为 `mock` 的真实 HTTP Provider Adapter。它只连接显式数字 Loopback IP 上的本地 Mock Provider，不调用 Handler 内部函数，也不需要 Provider Secret。

核心边界：

- Factory 只接受 `http://127.0.0.0/8` 或 `http://[::1]`，拒绝 HTTPS、Hostname、远程 IP、任意 Path、disabled Catalog 记录和任何 Secret Reference。
- `BuildRequest` 先校验 `NormalizedRequest`，把消息、媒体引用、工具、Tool Choice、结构化输出和采样参数映射为 OpenAI-compatible JSON；不添加 Authorization。
- Mock 场景只通过最多 64 KiB 的严格 `provider_options` Schema 选择；未知 Option/Scenario、Stream 语义冲突与超范围 Delay 都返回 `ErrUnsupportedParameter`。
- Mock Provider 没有 Policy Label 协议映射，因此非空 `PolicyLabels` 会显式失败，不能静默删除可能影响结果的策略字段。
- 普通响应最大 1 MiB，顶层/Choice/Message/Tool 字段使用白名单；未知普通响应字段产生 `ErrProtocol`，未知 Usage 字段则按计费契约保留精确 Raw Evidence 与排序 JSON Pointer。
- SSE Parser 限制单行 64 KiB、事件 256 KiB；Role/Content/Reasoning/Tool/Extension 分离为不同 Chunk，未知 Event 字段隔离到 `provider_extension`。
- Finish Event 会等待后续 Usage Event 或 `[DONE]`，最终输出 `usage_status=present|missing`，不会把“尚未收到”折叠为 0；缺少 `[DONE]`、重复 Finish/Usage、超大行和错误 JSON均为协议失败。
- `Next(ctx)` 取消会关闭 Body，解除阻塞网络读取并传播取消到 Mock Provider；`Close` 幂等。
- 429/503/4xx/5xx/Timeout 映射为有限 `NormalizedError`；原始 Provider Message 不进入 Error，Retry-After 只接受 24 小时内的秒数或 HTTP Date。
- 本 Adapter 不伪造本地 Tokenizer 估算，`EstimateUsage` 返回 `ErrUsageEstimationUnavailable`；账务事实来自 Provider Usage Fixture。

P05-T05 已将跨 Adapter 的真实 HTTP 行为抽到 `internal/adapterconformance`；本包通过注册 Fixture 运行统一矩阵，同时保留 Mock Options、大小限制和 SSE 异常状态机专项测试。

P05-T06 中 `ProtocolError` 实现安全 `provideradapter.ProtocolViolation`；`protocolcanary` 只读取 Operation/Code，将未知响应结构转换为内容无关 Drift Finding。
