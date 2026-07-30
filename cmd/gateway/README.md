# gateway

数据面 HTTP 进程。当前负责配置校验、监听器生命周期、健康探针、有限时优雅关闭和 `/v1/*` Virtual Key 认证；P04～P13 继续加入模型目录、策略、限流、预算、路由、代理、流式、计量事件和遥测。

## 启动

从项目根目录执行：

```powershell
Copy-Item .env.example .env  # 仅首次需要
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action gateway
```

默认监听 `:8080`。启动前会调用统一配置加载器；缺少 `DATABASE_URL`、非开发环境缺少 `VIRTUAL_KEY_HASH_KEY`、地址格式错误或生命周期超时非法时，进程拒绝启动。`.env` 只由本地脚本导入，应用本身不会隐式读取文件。

## 数据面认证

- 每个 `/v1/*` 请求必须恰好包含一个 `Authorization: Bearer <virtual-credential>`。
- Gateway 用安全前缀查询 PostgreSQL，再按 `hash_key_version` 选择 HMAC Keyring 项并常量时间比较摘要。
- Tenant、Project 必须为 `active`；Key 必须未吊销、未绝对过期，并且轮换旧 Key 尚在宽限期内。
- `X-Tenant-Id`、`X-Project-Id`、`X-Virtual-Key-Id` 等客户端自报身份会在进入业务 Handler 前删除；可信作用域只从数据库 Principal 取得。
- 缺失、格式错误、未知、错误 Secret、吊销、过期和作用域禁用统一返回 401 `INVALID_API_KEY`，不泄漏哪一步失败。
- 数据库故障或缺少摘要历史版本时 fail closed，返回可重试 503 `AUTHENTICATION_UNAVAILABLE`。

正缓存默认 TTL 为 2 秒、最大允许 30 秒；`0s` 可完全禁用。吊销/状态变更消费者应按安全前缀调用显式失效；绝对过期和轮换宽限每次按当前时钟重新判断，不会被缓存 TTL 延长。跨进程主动失效总线接入前，运维必须把 TTL 当作吊销最大陈旧窗口。

## 健康探针

| 路径 | 成功状态 | 语义 |
|---|---:|---|
| `GET /health/live` | 200 | 进程存活；不依赖 readiness 状态。 |
| `GET /health/ready` | 200 | HTTP 监听器已经启动，且尚未进入关闭流程。 |

非 GET 方法返回 405。readiness 尚未建立或关闭已经开始时返回 503、`Retry-After: 1` 和 `GATEWAY_NOT_READY` 错误。当前 readiness 仍只表示 HTTP 生命周期；认证查询会独立 fail closed。P13 上线门禁前将 PostgreSQL 等必需依赖纳入聚合探针。

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
