# gateway

数据面 HTTP 进程。当前骨架负责配置校验、监听器生命周期、健康探针与有限时优雅关闭；P04～P13 再逐步加入认证、策略、限流、预算、路由、代理、流式、计量事件和遥测。

## 启动

从项目根目录执行：

```powershell
Copy-Item .env.example .env  # 仅首次需要
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action gateway
```

默认监听 `:8080`。启动前会调用统一配置加载器；缺少 `DATABASE_URL`、地址格式错误或生命周期超时非法时，进程拒绝启动。`.env` 只由本地脚本导入，应用本身不会隐式读取文件。

## 健康探针

| 路径 | 成功状态 | 语义 |
|---|---:|---|
| `GET /health/live` | 200 | 进程存活；不依赖 readiness 状态。 |
| `GET /health/ready` | 200 | HTTP 监听器已经启动，且尚未进入关闭流程。 |

非 GET 方法返回 405。readiness 尚未建立或关闭已经开始时返回 503、`Retry-After: 1` 和 `GATEWAY_NOT_READY` 错误。当前 P03-T01 不探测 PostgreSQL、Redis、Kafka、ClickHouse 或 Provider；相关依赖接入后再将其纳入 readiness 聚合，避免产生虚假的依赖健康结论。

本地验证：

```powershell
Invoke-RestMethod http://localhost:8080/health/live
Invoke-RestMethod http://localhost:8080/health/ready
```

## 关闭语义

进程监听 `Ctrl+C`、Windows interrupt 与 `SIGTERM`。收到停止信号后按以下顺序执行：

1. 立即将 readiness 置为不可用。
2. 关闭监听器，拒绝新连接。
3. 在 `SHUTDOWN_TIMEOUT`（默认 15 秒）内等待普通在途请求完成。
4. 超时则强制关闭连接并返回非零退出码，确保部署系统能够观察到未完成的排空。

SSE/升级连接的主动登记、取消与排空策略属于 P03-T08，本任务不提前伪装为已经覆盖。
