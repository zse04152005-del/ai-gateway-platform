# API 与事件文档

P01 将在本目录创建公开 OpenAPI 草案、统一错误码、SSE 事件约定和异步事件 Envelope。

兼容性原则：

- 明确支持的 OpenAI-compatible 字段和端点。
- 不支持的供应商特性返回可识别错误，不静默忽略影响语义或计费的字段。
- SSE、普通响应和错误必须具备自动化 Fixture。

