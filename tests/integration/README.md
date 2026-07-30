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
