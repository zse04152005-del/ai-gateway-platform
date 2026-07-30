# control-plane

控制面 HTTP 进程。负责租户、项目、虚拟 Key、Provider、模型、Deployment、价格、路由和配置发布；当前已提供进程状态与 P04 虚拟 Key 生命周期 API。

## 启动

从项目根目录执行：

```powershell
Copy-Item .env.example .env  # 仅首次需要
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action control-plane
```

默认监听 `:8081`，与数据面 gateway 的 `:8080` 隔离。启动前使用统一配置加载器校验环境，应用本身不会隐式读取 `.env`。

## 当前路由

| 路径 | 状态 | 语义 |
|---|---:|---|
| `GET /health/live` | 200 | 进程存活。 |
| `GET /health/ready` | 200/503 | 监听器已启动且尚未进入关闭流程。 |
| `GET /admin/v1/status` | 200 | 返回 `status`、`service` 与构建版本，不包含配置、租户或密钥。 |
| `POST /admin/v1/tenants/{tenant}/projects/{project}/virtual-keys` | 201 | 创建 Key；完整凭据只在本响应返回一次。 |
| `GET /admin/v1/tenants/{tenant}/projects/{project}/virtual-keys/{id}` | 200 | 查询安全元数据和派生有效状态，不返回凭据或摘要。 |
| `POST .../virtual-keys/{id}/rotate` | 201 | 在一个 PostgreSQL 事务内创建替代 Key 并启动旧 Key 宽限期。 |
| `POST .../virtual-keys/{id}/revoke` | 200 | 永久吊销；重复调用保留原始吊销事实。 |

本地验证：

```powershell
Invoke-RestMethod http://localhost:8081/health/live
Invoke-RestMethod http://localhost:8081/health/ready
Invoke-RestMethod http://localhost:8081/admin/v1/status
```

虚拟 Key 写操作要求 `X-Admin-Actor` 审计 Header，完整契约见根目录 OpenAPI。当前 Header 由可信管理边界提供，不是身份凭据；生产管理面 OIDC、audience 与角色授权仍按 P12-T01 接入，在此之前不得把控制面直接暴露到公网。

控制面启动需要版本化 HMAC 摘要密钥。开发环境在 `VIRTUAL_KEY_HASH_KEY` 留空时，从 `LOCAL_ENVELOPE_KEY` 进行带上下文标签的域分离派生；测试、预发布和生产必须显式提供 32 字节十六进制 `VIRTUAL_KEY_HASH_KEY`。

## 关闭

与 gateway 共用 `internal/httpserver` 生命周期：收到 interrupt 或 `SIGTERM` 后先撤销 readiness、停止新连接，再在 `SHUTDOWN_TIMEOUT` 内排空普通在途请求；超时强制关闭并返回非零退出码。
