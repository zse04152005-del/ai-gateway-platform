# P06-T05 普通响应与统一输出验收报告

## 1. 结论

P06-T05 已在工作副本完成代码、专项测试和完整本地门禁。`POST /v1/chat/completions` 的 `stream=false` 路径现已接通认证、严格解析、规范化、可信 Selection、Deployment-scoped Adapter、共享上游 HTTP Client、有界响应解析和统一客户端 JSON。

`stream=true` 仍明确返回 501 `CHAT_STREAMING_NOT_IMPLEMENTED`，不会静默退化为普通响应。GatewayRequest/RouteAttempt 持久化属于紧接着的 P06-T06，本报告不把它伪装为已经完成。

## 2. 交付内容

### 2.1 单 Attempt 执行边界

- 新增 `internal/proxy.NonStreamExecutor`，一次调用只执行一个 `routing.Selection`。
- 直接使用 Selection 内同一快照的 Provider/Deployment 构建 Adapter，不二次查询目录。
- Adapter Registry 在 Gateway 启动时显式注册 `mock` 与 `openai`，未知类型 fail closed。
- Mock 调用不需要凭据；OpenAI 在 development 可通过 PostgreSQL Secret Reference + Local Envelope Manager 最短解析凭据。
- 没有 Local Resolver 的环境仍可启动 Mock 路径，但 OpenAI 凭据解析稳定失败为后端不可用；不会读取明文环境 Provider Key。Vault/KMS Adapter 保留到 P12-T03。
- Adapter 负责有界读取并关闭响应 Body；Executor 再做防御性 Close。
- 成功结果再次执行 `NormalizedResponse.Validate()`，且物理模型必须与选定 Deployment 一致。

### 2.2 统一成功响应

- `model` 使用客户端请求的 Logical Model，不暴露物理模型、Deployment、Endpoint 或 Provider Request ID。
- Assistant 文本、Tool Call ID/Name/Arguments 和所有有限 Finish Reason 正确投影。
- `content_policy` 对外兼容为 `content_filter`；未知供应商 Finish 只输出 `unknown`，不回显原始原因。
- Input/Output Token 都明确存在时才输出 Usage；`total_tokens` 由二者求和。
- Cache Read/Write、Reasoning 和 Audio Token 使用 presence-preserving 可选字段，真实 0 与缺失不混淆。
- Provider Usage 缺少核心计数时省略公共 Usage，并以 `gateway.usage_complete=false` 明确说明，不制造全 0 计量。
- `gateway.request_id`、当前真实单次 `attempt_count=1` 和 Usage 完整性随响应返回。

### 2.3 统一错误

- Provider 429 → 429 `PROVIDER_RATE_LIMITED`。
- Provider capacity/5xx → 503 `PROVIDER_UNAVAILABLE`。
- Provider timeout/Transport deadline → 504 `PROVIDER_TIMEOUT`。
- Provider credential/permission → 502 `PROVIDER_CREDENTIAL_ERROR`，不会误报为客户端 Virtual Key 401。
- Provider invalid normalized request → 502 `PROVIDER_REQUEST_REJECTED`。
- Content policy → 403 `CONTENT_POLICY_REJECTED`。
- 连接失败 → 502 `PROVIDER_CONNECTION_FAILED`。
- 协议/媒体类型/JSON/模型身份错误 → 502 `PROVIDER_PROTOCOL_ERROR`。
- 调用方取消 → 499 `REQUEST_CANCELLED`；P06-T07 补充 Attempt 状态记录。

公共错误只依赖有限分类，不包含 Provider Body/Message、网络 Origin、数据库 Cause 或 Secret Locator。`ProviderError` 只能由已通过 `NormalizedError.Validate()` 的事实构造；内部 `executionError.Error()` 只返回稳定类别，私有 cause 仅通过 `errors.Is/As` 供可信控制流使用。

## 3. 自动化验证

### 3.1 真实 HTTP 与协议用例

`internal/proxy` 使用真实 `httptest.Server`、Mock Provider、Mock Adapter Registry 和共享 `upstreamhttp.Client` 验证：

- normal 普通响应与 Stop Finish。
- cached-usage 的 Input/Output/Cache Read。
- tool-call 的 `get_weather`、合法 JSON Arguments 与 Tool Calls Finish。
- 429 的 RateLimit/RetryAfter 安全归一化。
- 503 的 Capacity 安全归一化。
- 截断 JSON 的 Protocol 分类。
- HTTP Transport 私有错误不进入 `Error()`，但保留 `errors.Is`。
- Context 取消、stream=true、Logical Model 不匹配和 nil 依赖 fail closed。

`internal/gateway` 验证：

- 完整 Handler 成功响应的 Model、Tool Calls、Finish、Usage Details 和 Gateway Metadata。
- 客户端 Prompt 不出现在响应元数据。
- Provider Model 与 Provider Request ID 不进入公共 JSON。
- RateLimit、Capacity、Provider Auth、Timeout、Protocol 与 Cancellation 的稳定 HTTP 映射。
- 私有 Provider Message/Body Marker 不进入错误 Envelope。
- 所有 Finish Reason 映射和缺失 Usage 语义。
- `stream=true` 不调用普通 Executor。

### 3.2 覆盖率与稳定性

- `internal/proxy`：`83.1%` statements。
- `internal/gateway`：`82.8%` statements。
- `proxy/gateway/mockadapter/openaiadapter/upstreamhttp/cmd/gateway`：`20/20` 轮稳定测试通过。
- 专项 `go vet` 通过，golangci-lint `0 issues`。

### 3.3 完整仓库门禁

执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action check
```

模块校验、格式、vet、普通与 integration build-tag 双 lint、全包测试、全包构建、`govulncheck`、6 个迁移顺序、CI Workflow Actionlint 和高风险秘密扫描全部通过；`govulncheck` 输出 `No vulnerabilities found.`。OpenAPI YAML 由远端 `config-and-secrets` Job 使用仓库 `.yamllint.yml` 独立门禁。

## 4. OpenAPI 与文档

- `ChatCompletionResponse` 现在要求 Gateway Metadata，并严格声明 Tool Call 和 Finish Reason。
- Usage Schema 补齐 Cache Write、Reasoning、Audio Details 与 Source。
- 路径补充 499 取消和 P07 前的 501 流式未实现响应。
- 新增非流式执行 API 契约、架构责任边界，并更新 Gateway/内部模块/根 README 与 Changelog。

## 5. 下一步

P06-T06 在 Executor 外层建立 GatewayRequest 和独立 RouteAttempt，并把状态迁移、Selection、Provider/Deployment、开始/结束时间与本次统一结果关联。只有完成该事实层后，P06-T07 的客户端取消才能可靠记录为 `client_cancelled`，P08 的重试也才能做到一次上游收费对应一个 Attempt。
