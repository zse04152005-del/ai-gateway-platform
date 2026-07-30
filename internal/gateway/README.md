# gateway

数据面应用路由装配层。共享 `httpserver` 负责健康、关联与连接生命周期；本模块负责把所有 `/v1/*` 路由放在 `keyauth.Authenticator` 之后。

`GET /v1/models` 已实现项目白名单、Key 收窄策略与目录可用性过滤，只返回稳定逻辑模型及客户端能力；不返回 Provider、物理模型、Deployment、Endpoint 或其他租户信息。目录不可可信时 fail closed 为 503 `MODEL_CATALOG_UNAVAILABLE`。

`POST /v1/chat/completions` 的非流式路径已接通严格解析、规范化、可信选路、单次 Provider 执行与统一 JSON。公共 `model` 始终是逻辑模型；Usage 不把缺失计量伪造为 0，Tool Call Arguments 保持 JSON 字符串，Provider Body/Endpoint/物理模型和私有错误不进入响应。`stream=true` 在 P07 前明确返回 501，不静默降级。

其他未实现业务端点返回安全 JSON 404。非 `/v1/*` 未知路径不触发数据库认证，避免扫描流量消耗认证资源。
