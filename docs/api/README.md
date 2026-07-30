# API 与事件文档

本目录维护公开 OpenAPI、统一错误/重试语义、Provider Adapter 契约和异步事件 Envelope。

当前核心文档：

- [`provider-adapter-contract.md`](./provider-adapter-contract.md)：Adapter 职责、能力和一致性矩阵。
- [`normalized-provider-types.md`](./normalized-provider-types.md)：P05 已实现的 Request/Response/Chunk/Error/Usage 可执行类型契约。
- [`error-and-retry-semantics.md`](./error-and-retry-semantics.md)：公开错误、重试与部分失败语义。

兼容性原则：

- 明确支持的 OpenAI-compatible 字段和端点。
- 不支持的供应商特性返回可识别错误，不静默忽略影响语义或计费的字段。
- SSE、普通响应和错误必须具备自动化 Fixture。
