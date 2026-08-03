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
- `redis_tpm_test.go` 在真实 Redis 7.4 上以 64 路并发验证四层 TPM 最大量预留严格不超 hard、相同 ID 幂等、并发结算释放差额、实际超额补记并阻止后续 admission、绝对 TTL 不滑动，以及过期/冲突/损坏状态 fail closed；CI migration-integration Job 强制执行。
- `redis_concurrency_test.go` 在真实 Redis 7.4 上以 64 路并发验证四层 ZSET Lease hard 不超卖、soft/最早过期事实、幂等 Acquire/Release、长请求 Renew、正常/失败/取消显式清理、模拟进程退出后的自动过期恢复，以及损坏成员无部分释放；CI migration-integration Job 强制执行。
- `budget_ledger_schema_test.go` 在真实 PostgreSQL 上验证 Tenant/Project/Key/User/Session 五种独立账户形状与周期唯一性、跨租户复合外键、精确金额边界、Reservation 单向终态、账户版本推进和 Ledger 追加不可变性；CI migration-integration Job 强制执行。
- `budget_atomic_reservation_test.go` 在真实 PostgreSQL 上以 160 路并发验证 Account version CAS 不突破 hard、soft 事实、幂等重放、冲突 Key、跨租户隐藏、FK 失败全事务回滚，以及受控行锁下 1 次重试封顶与第 2 次成功；CI migration-integration Job 强制执行。
- `budget_settlement_test.go` 在真实 PostgreSQL 上验证关闭账户多 Attempt 成功、失败超预留/hard、缓存命中、零费用取消和部分费用取消，并以 64 路并发证明同一 Reservation 只结算一次、其余调用读取相同终态 Ledger；冲突重放、未知 Reservation、Request Outcome 不匹配和每账户两条 Ledger 上限同时覆盖；CI migration-integration Job 强制执行。
- `budget_reaper_test.go` 在真实 PostgreSQL 上以 8 个并发 Worker 和 batch=3 验证 16 个过期 Reservation 的 `FOR UPDATE SKIP LOCKED` 分片回收、唯一 expire Ledger/EventID、未来 hold 保留、closed Account 释放和重复扫描空结果，并证明 Settler/Reaper 同时竞争时只能产生一个终态；CI migration-integration Job 强制执行。
- `budget_limit_notice_test.go` 在真实 PostgreSQL 上验证恰好 soft 无告警、超过 soft 返回 remaining/reset/hint、hard 拒绝返回可 `errors.As` 的结构化安全错误、拒绝无 Reservation/Ledger 副作用，以及伪造其他 Tenant 时不返回任何额度或资源身份；CI migration-integration Job 强制执行。
- `usage_ledger_schema_test.go` 在真实 PostgreSQL 上验证 Request/Attempt 用量归属、Request 级可空 Attempt、`event_id` 跨 Request 全局唯一、Tenant/Request/Attempt 复合外键、整数数量与安全标识符边界、UPDATE/DELETE 追加写保护，以及敏感内容列缺失；CI migration-integration Job 强制执行。
- `usage_taxonomy_test.go` 将 `internal/metering` 的 9 个 Token 类型与 4 个来源全部写入真实 PostgreSQL，并拒绝大小写、空白、合计类型、无方向媒体类型和供应商自定义值，防止 Go 与 Schema 枚举漂移；CI migration-integration Job 强制执行。
