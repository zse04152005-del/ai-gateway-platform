# adapterconformance

`adapterconformance` 是 Provider Adapter 的统一真实 HTTP 契约测试框架。新增 Adapter 不复制测试逻辑，只注册部署构造器、协议 Handler Fixture、输入请求和期望的 Normalized Facts。

统一套件强制覆盖：

- 普通响应和完整 Provider Usage；
- SSE 起始、内容、终态、Usage、严格 EOF；
- 阻塞读取取消、响应 Body 关闭和上游 Context 传播；
- 429 与可重试 5xx/Capacity 错误，及敏感 Provider Message 不泄露；
- Cache Read Token、Tool Call；
- length、content policy、未知 finish reason；
- 普通响应未知字段 fail closed、流式未知字段隔离为 `provider_extension`。

`Registration.Validate` 在启动测试服务器前拒绝缺失、重复、非法或语义不完整的 Fixture，不能用空切片跳过必测行为。每个 Case 使用新的共享 `httpserver` Handler、真实 `httptest.Server` 和新的 Adapter，`BuildRequest` 生成的 URL 必须保持在隔离 Fixture Origin 内。

套件还校验输入 Request 不被修改、所有 Normalized 值通过领域 `Validate`、Chunk Sequence 从 0 单调递增、终态后持续返回 EOF，以及错误中不出现注册的合成敏感标记。

具体注册示例位于 `internal/mockadapter/conformance_test.go`，完整契约见 `docs/api/adapter-conformance-suite.md`。
