# mockprovider

本地、确定性、OpenAI-compatible 协议模拟器：

- `POST /v1/chat/completions` 支持普通 JSON、SSE、固定/缓存 Usage、Tool Call、延迟、429、503、截断连接和错误 Chunk。
- 场景由 `mock_scenario`、`X-Mock-Scenario` 或 `?scenario=` 精确选择；多入口必须一致。
- 请求最大 1 MiB，延迟 1～5000 ms，所有等待和 SSE 都响应 Context 取消。
- SSE 请求向共享 `httpserver` 注册为 Stream，进程关闭时立即取消。
- 输出不复制 Prompt，不访问外部依赖，不读取或记录 Provider 凭据。

公开场景契约见 [`docs/development/mock-provider.md`](../../docs/development/mock-provider.md)。
