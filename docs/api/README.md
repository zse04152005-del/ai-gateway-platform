# API 与事件文档

本目录维护公开 OpenAPI、统一错误/重试语义、Provider Adapter 契约和异步事件 Envelope。

当前核心文档：

- [`provider-adapter-contract.md`](./provider-adapter-contract.md)：Adapter 职责、能力和一致性矩阵。
- [`provider-adapter-registry.md`](./provider-adapter-registry.md)：显式 Factory 注册、启动/发布未知类型门禁与安全失败语义。
- [`mock-adapter.md`](./mock-adapter.md)：真实 HTTP Mock Adapter、Usage 证据映射、SSE 状态机和错误语义。
- [`adapter-conformance-suite.md`](./adapter-conformance-suite.md)：统一真实 HTTP Fixture 注册、强制矩阵、取消和错误安全断言。
- [`protocol-canary.md`](./protocol-canary.md)：最小成本真实协议探针、漂移 Finding、安全结果和周期调度边界。
- [`normalized-provider-types.md`](./normalized-provider-types.md)：P05 已实现的 Request/Response/Chunk/Error/Usage 可执行类型契约。
- [`non-stream-chat-execution.md`](./non-stream-chat-execution.md)：单 Attempt 普通代理、统一响应、Usage/Finish/Tool Call 与安全错误映射。
- [`sse-heartbeat.md`](./sse-heartbeat.md)：可选 Gateway SSE comment 心跳、客户端开关、频率治理和超时语义隔离。
- [`error-and-retry-semantics.md`](./error-and-retry-semantics.md)：公开错误、重试与部分失败语义。

兼容性原则：

- 明确支持的 OpenAI-compatible 字段和端点。
- 不支持的供应商特性返回可识别错误，不静默忽略影响语义或计费的字段。
- SSE、普通响应和错误必须具备自动化 Fixture。
