# control-plane

控制面 HTTP 进程。后续负责租户、项目、虚拟 Key、Provider、模型、Deployment、价格、路由和配置发布；当前 P03-T02 只建立独立监听、健康探针、基础状态路由与有限时优雅关闭。

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

本地验证：

```powershell
Invoke-RestMethod http://localhost:8081/health/live
Invoke-RestMethod http://localhost:8081/health/ready
Invoke-RestMethod http://localhost:8081/admin/v1/status
```

当前状态路由是无敏感信息的启动骨架。租户、模型和 Key 等管理 API 不会在身份与 RBAC 完成前伪装为可用；管理面 OIDC、audience 与角色授权按 P12-T01 实现。

## 关闭

与 gateway 共用 `internal/httpserver` 生命周期：收到 interrupt 或 `SIGTERM` 后先撤销 readiness、停止新连接，再在 `SHUTDOWN_TIMEOUT` 内排空普通在途请求；超时强制关闭并返回非零退出码。
