# 集成测试

使用 Go build tag `integration` 和可抛弃依赖环境。测试必须可重复，不依赖开发者个人数据或真实供应商 Key。

统一入口：

```text
make test-integration
powershell -ExecutionPolicy Bypass -File scripts/dev.ps1 -Action test-integration
```

当前模板：

- `process_lifecycle_test.go` 在临时目录构建三个核心进程和本地 `mock-provider` 共四个真实二进制，使用随机本地端口和完全受控的测试环境变量。
- gateway/control-plane/mock-provider 验证启动、HTTP 健康/状态、关联 Header、SIGTERM 干净退出及结构化日志。
- metering-worker 使用测试内 TCP Listener 作为 bootstrap 依赖，连接成功代表当前骨架健康，并验证 SIGTERM 后连接关闭。
- 四个二进制都执行无效配置负向测试，必须非零退出、输出 JSON 稳定错误码且不泄露内部 URL/凭据；mock-provider 额外验证生产环境拒绝启动。
- Windows 本地因 Go 无法向子进程发送真实 SIGTERM 而跳过实际信号测试，并自动省略当前无 CGO 环境不支持的 `-race`；命令层单测仍覆盖 Context 取消，Linux CI 强制以 race detector 运行完整模板。
- 真实 Redpanda 连通测试在设置 `KAFKA_BROKERS` 时运行，否则明确 Skip。
- `tenant_project_schema_test.go` 在 `DATABASE_URL` 已迁移到最新版本时验证 Tenant/Project 数据库约束；CI PostgreSQL Job 强制执行。
- `virtual_api_key_schema_test.go` 验证 Virtual API Key 不含明文字段、32 字节摘要、Tenant/Project 复合隔离、前缀/摘要唯一、白名单/限额格式和轮换/吊销状态约束；CI PostgreSQL Job 强制执行。
- `virtual_key_lifecycle_test.go` 使用真实 PostgreSQL 验证一次性创建、事务轮换、并发单替代者、授权/限额继承、幂等吊销和派生过期；完整凭据必须在数据库 JSON 表示中不存在。
- `virtual_key_auth_test.go` 贯通签发、PostgreSQL 查询和 HTTP Middleware，验证错误 Secret、未知前缀、吊销、绝对过期、轮换宽限、Tenant/Project 禁用、伪造作用域 Header 和缓存显式失效。
- `model_catalog_schema_test.go` 用真实 PostgreSQL 验证逻辑/物理模型分离、租户唯一性、严格 Capability/Region/Endpoint 约束、绑定兼容性触发器、配置漂移防护和无凭据列。
- `model_list_test.go` 贯通真实 Key 签发、认证、项目/Key 白名单三态与 `/v1/models`，验证 active Provider/Deployment 过滤、跨租户复合外键、伪造 Header 和响应不泄漏物理目录。
- `provider_secret_test.go` 用真实 PostgreSQL 验证 AES-GCM Envelope 明文不落库、同 Provider 绑定、跨 Provider 拒绝、Vault Locator 互斥材料、禁用解析和无明文字段。
- `tenant_isolation_test.go` 建立两个完整租户链路，验证 Key 直接 ID/轮换/吊销、混合 Tenant/Project 列表、缓存深拷贝与前缀绑定、伪造身份 Header、`/v1/models`、Provider Secret/Deployment 复合引用和统一 401 均不能跨边界；CI PostgreSQL Job 强制执行。
- `gateway_execution_test.go` 在真实 PostgreSQL 上验证成功/Provider 失败生命周期、CAS 冲突、Attempt 序号唯一、跨作用域外键、非法跃迁、终态不可覆盖及按版本追加的状态事件；CI PostgreSQL Job 强制执行。
- `non_stream_e2e_test.go` 贯通真实 HTTP Server、Key 签发/认证、PostgreSQL Catalog/路由/执行记录、Mock Adapter、共享上游 Client 和真实 Mock Provider，覆盖成功、认证失败、未知模型、超时、429、5xx 与客户端取消，并核对 Request/Attempt 终态、Header 事实和追加式事件；CI PostgreSQL Job 强制执行。
- `redis_rpm_test.go` 在真实 Redis 7.4 上以 128 路并发验证 Platform/Tenant/Project/Key 四层 Lua 的全有或全无计数、服务器分钟边界纠正、绝对 TTL、soft 事实、hard 拒绝和损坏计数 fail closed；CI migration-integration Job 强制执行。
