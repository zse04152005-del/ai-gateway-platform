# 集成测试

使用 Go build tag `integration` 和可抛弃依赖环境。测试必须可重复，不依赖开发者个人数据或真实供应商 Key。

统一入口：

```text
make test-integration
powershell -ExecutionPolicy Bypass -File scripts/dev.ps1 -Action test-integration
```

当前模板：

- `process_lifecycle_test.go` 在临时目录构建三个真实二进制，使用随机本地端口和完全受控的测试环境变量。
- gateway/control-plane 验证启动、HTTP 健康/状态、关联 Header、SIGTERM 干净退出及结构化日志。
- metering-worker 使用测试内 TCP Listener 作为 bootstrap 依赖，连接成功代表当前骨架健康，并验证 SIGTERM 后连接关闭。
- 三个二进制都执行无效配置负向测试，必须非零退出、输出 JSON 稳定错误码且不泄露内部 URL/凭据。
- Windows 本地因 Go 无法向子进程发送真实 SIGTERM 而跳过实际信号测试，并自动省略当前无 CGO 环境不支持的 `-race`；命令层单测仍覆盖 Context 取消，Linux CI 强制以 race detector 运行完整模板。
- 真实 Redpanda 连通测试在设置 `KAFKA_BROKERS` 时运行，否则明确 Skip。
- `tenant_project_schema_test.go` 在 `DATABASE_URL` 已迁移到最新版本时验证 Tenant/Project 数据库约束；CI PostgreSQL Job 强制执行。
- `virtual_api_key_schema_test.go` 验证 Virtual API Key 不含明文字段、32 字节摘要、Tenant/Project 复合隔离、前缀/摘要唯一、白名单/限额格式和轮换/吊销状态约束；CI PostgreSQL Job 强制执行。
- `virtual_key_lifecycle_test.go` 使用真实 PostgreSQL 验证一次性创建、事务轮换、并发单替代者、授权/限额继承、幂等吊销和派生过期；完整凭据必须在数据库 JSON 表示中不存在。
- `virtual_key_auth_test.go` 贯通签发、PostgreSQL 查询和 HTTP Middleware，验证错误 Secret、未知前缀、吊销、绝对过期、轮换宽限、Tenant/Project 禁用、伪造作用域 Header 和缓存显式失效。
- `model_catalog_schema_test.go` 用真实 PostgreSQL 验证逻辑/物理模型分离、租户唯一性、严格 Capability/Region/Endpoint 约束、绑定兼容性触发器、配置漂移防护和无凭据列。
- `model_list_test.go` 贯通真实 Key 签发、认证、项目/Key 白名单三态与 `/v1/models`，验证 active Provider/Deployment 过滤、跨租户复合外键、伪造 Header 和响应不泄漏物理目录。
- `provider_secret_test.go` 用真实 PostgreSQL 验证 AES-GCM Envelope 明文不落库、同 Provider 绑定、跨 Provider 拒绝、Vault Locator 互斥材料、禁用解析和无明文字段。
