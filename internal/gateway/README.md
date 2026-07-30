# gateway

数据面应用路由装配层。共享 `httpserver` 负责健康、关联与连接生命周期；本模块负责把所有 `/v1/*` 路由放在 `keyauth.Authenticator` 之后。

`GET /v1/models` 已实现项目白名单、Key 收窄策略与目录可用性过滤，只返回稳定逻辑模型及客户端能力；不返回 Provider、物理模型、Deployment、Endpoint 或其他租户信息。目录不可可信时 fail closed 为 503 `MODEL_CATALOG_UNAVAILABLE`。

其他未实现业务端点返回安全 JSON 404。非 `/v1/*` 未知路径不触发数据库认证，避免扫描流量消耗认证资源。
