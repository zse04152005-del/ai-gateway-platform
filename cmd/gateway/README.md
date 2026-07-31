# gateway

数据面 HTTP 进程。当前负责配置校验、监听器生命周期、健康探针、有限时优雅关闭和 `/v1/*` Virtual Key 认证；P04～P13 继续加入模型目录、策略、限流、预算、路由、代理、流式、计量事件和遥测。

## 启动

从项目根目录执行：

```powershell
Copy-Item .env.example .env  # 仅首次需要
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action gateway
```

默认监听 `:8080`。启动前会调用统一配置加载器；缺少 `DATABASE_URL`、非开发环境缺少 `VIRTUAL_KEY_HASH_KEY`、地址格式错误或生命周期超时非法时，进程拒绝启动。`.env` 只由本地脚本导入，应用本身不会隐式读取文件。

进程同时创建唯一的共享上游 HTTP Client。连接/TLS/响应头/非流式总超时、连接池和响应 Header 上限均由 `UPSTREAM_HTTP_*` 配置控制；关闭时主动释放 idle 连接。Client 不读取环境代理、不跟随 Provider 重定向，并在发送前剥离客户端代理链、Cookie 和 hop-by-hop Header。Provider Adapter 生成的供应商认证 Header 保留在独立的出站请求中，入站 Virtual Key 从不复制到该请求。

P08-T04 另建独立的主动健康 HTTP Client/Transport，不复用生产连接池：每 Host 最多 2 个连接，全局调度最多 4 个并发、每批最多 16 个，单次最多 5 秒。探针固定发送 `ping`、最多请求 1 个输出 Token，并标记 `X-AI-Gateway-Traffic-Class: active-health/v1`。它不经过公开 Handler、Virtual Key、业务 Attempt、被动统计或计费记录；有真实被动健康样本的热 Deployment 会暂停主动探针，只检查冷/备用路由。Provider 侧若需要完全独立的额度或账单，仍应为 Probe Secret Reference 配置独立供应商凭据；当前目录模型不会声称同一 Provider 账户内存在虚假的配额隔离。

非流式 Chat 已注册 `mock` 与 `openai` Factory，并按选定 Deployment 构建 Adapter、执行一次共享 HTTP 调用、解析 Normalized Response，再投影为统一 OpenAI-compatible JSON。开发环境的 OpenAI 凭据通过 PostgreSQL Secret Reference 和本地 Envelope Manager 在请求构造最短边界解析；未配置 Resolver 时 fail closed，不允许从环境变量或目录记录读取明文 Provider Key。

## 数据面认证

- 每个 `/v1/*` 请求必须恰好包含一个 `Authorization: Bearer <virtual-credential>`。
- Gateway 用安全前缀查询 PostgreSQL，再按 `hash_key_version` 选择 HMAC Keyring 项并常量时间比较摘要。
- Tenant、Project 必须为 `active`；Key 必须未吊销、未绝对过期，并且轮换旧 Key 尚在宽限期内。
- `X-Tenant-Id`、`X-Project-Id`、`X-Virtual-Key-Id` 等客户端自报身份会在进入业务 Handler 前删除；可信作用域只从数据库 Principal 取得。
- 缺失、格式错误、未知、错误 Secret、吊销、过期和作用域禁用统一返回 401 `INVALID_API_KEY`，不泄漏哪一步失败。
- 数据库故障或缺少摘要历史版本时 fail closed，返回可重试 503 `AUTHENTICATION_UNAVAILABLE`。

正缓存默认 TTL 为 2 秒、最大允许 30 秒；`0s` 可完全禁用。吊销/状态变更消费者应按安全前缀调用显式失效；绝对过期和轮换宽限每次按当前时钟重新判断，不会被缓存 TTL 延长。跨进程主动失效总线接入前，运维必须把 TTL 当作吊销最大陈旧窗口。

## 模型列表

`GET /v1/models` 在认证后计算 Project 授权、Key 白名单和目录 active 状态的交集。Key 白名单为 `NULL` 时继承项目、空数组时拒绝全部、非空时只能收窄；成功响应不暴露 Provider/Deployment/Endpoint。目录查询失败返回可重试 503 `MODEL_CATALOG_UNAVAILABLE`。

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
3. 取消主动健康调度与在途探针，并关闭独立探针连接池。
4. 在 `SHUTDOWN_TIMEOUT`（默认 15 秒）内等待普通在途请求完成。
5. 超时则强制关闭连接并返回非零退出码，确保部署系统能够观察到未完成的排空。

SSE/升级连接的主动登记、取消与排空策略属于 P03-T08，本任务不提前伪装为已经覆盖。
