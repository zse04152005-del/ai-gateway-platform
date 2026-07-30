# 本地 Mock Provider 场景协议

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P05-T01

## 1. 目标与安全边界

`mock-provider` 是 OpenAI-compatible 的确定性协议模拟器，用于在没有外网和真实供应商凭据时开发 Adapter、代理、SSE、取消、重试、Usage 与工具调用链路。它不是生产降级 Provider，也不能产生业务回答。

- 只允许 `APP_ENV=development|test`。
- 监听地址必须是显式 Loopback（`127.0.0.0/8`、`::1` 或 `localhost`）。
- 配置加载不依赖 PostgreSQL、Redis、Kafka、ClickHouse、OTLP 或外网。
- 不校验或保存真实 Provider Key；请求正文不进入生命周期日志。
- 所有输出均为固定 Fixture，时间戳固定为 `0`，不依赖墙钟或随机数。
- 请求体最大 1 MiB；延迟最大 5 秒且响应客户端取消/进程关闭。

## 2. 启动

```powershell
Copy-Item .env.example .env
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action mock-provider
```

默认监听 `127.0.0.1:18082`：

```powershell
Invoke-RestMethod http://127.0.0.1:18082/health/ready
```

普通请求示例：

```powershell
$body = @{
  model = 'mock-chat'
  messages = @(@{ role = 'user'; content = 'hello' })
} | ConvertTo-Json -Depth 5

Invoke-RestMethod `
  -Method Post `
  -Uri http://127.0.0.1:18082/v1/chat/completions `
  -ContentType 'application/json' `
  -Body $body
```

## 3. 场景选择

场景可由以下任一入口指定：

1. JSON 字段：`mock_scenario`
2. Header：`X-Mock-Scenario`
3. Query：`?scenario=<id>`

多个入口同时出现时值必须完全一致，否则返回 `400 ambiguous_mock_scenario`。未指定时，`stream=false` 选择 `normal`，`stream=true` 选择 `sse`。响应 Header 会回显最终 `X-Mock-Scenario`，便于测试确认命中的 Fixture。

| 场景 ID | HTTP/协议行为 | 固定证据 |
|---|---|---|
| `normal` | 普通 JSON Completion | Usage `6 + 4 = 10`，`finish_reason=stop` |
| `sse` | 合法 SSE | role/content/finish/usage 五个 Chunk，最后 `[DONE]` |
| `fixed-usage` | 普通 JSON | Prompt 11、Completion 7、Total 18、Reasoning 2 |
| `cached-usage` | 普通 JSON | Prompt 13、Cached 5、Completion 3、Total 16 |
| `tool-call` | 普通 JSON Tool Call | `get_weather`、固定 JSON Arguments、`finish_reason=tool_calls` |
| `delay` | 延迟后返回普通或 SSE | `mock_delay_ms` 默认 100，允许 1～5000 |
| `rate-limit` | HTTP 429 | `Retry-After: 1`、`rate_limit_exceeded` |
| `server-error` | HTTP 503 | `mock_provider_unavailable` |
| `disconnect` | HTTP 200 后截断连接 | 声明较长 Content-Length，只写部分 JSON 后关闭连接 |
| `malformed-chunk` | HTTP 200 SSE | 先写合法 Chunk，再写不完整 JSON，不发送 `[DONE]` |

延迟请求示例：

```json
{
  "model": "mock-chat",
  "messages": [{"role": "user", "content": "hello"}],
  "mock_scenario": "delay",
  "mock_delay_ms": 250
}
```

## 4. 错误契约

Mock 返回 OpenAI 风格错误：

```json
{
  "error": {
    "message": "mock_scenario is not supported",
    "type": "invalid_request_error",
    "param": "mock_scenario",
    "code": "unknown_mock_scenario"
  }
}
```

稳定错误覆盖错误方法、路径、Content-Type、JSON、模型、消息、场景歧义、未知场景、延迟范围和请求大小。错误不回显请求正文或内部 cause。

## 5. 自动化验证

- `internal/mockprovider/handler_test.go` 通过真实 `httptest.Server` 覆盖每个场景、SSE Flush、截断连接、错误契约和取消。
- `cmd/mock-provider/main_test.go` 覆盖配置先验失败、监听参数、结构化日志与 Context 停止。
- `tests/integration/process_lifecycle_test.go` 在 Linux CI 构建真实二进制，验证 readiness、SIGTERM 退出码 0、JSON 日志和生产环境拒绝启动。
- P05-T04 的 Mock Adapter 已通过共享 HTTP 生命周期上的真实 `httptest.Server` 覆盖本文件全部场景，没有按测试名或进程内返回 Fixture；P05-T05 Conformance Suite 必须继续使用同一协议边界。
