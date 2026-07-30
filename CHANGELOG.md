# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的结构，并计划使用语义化版本。正式发布前保持 `0.x` 版本。

## [Unreleased]

### Added

- 建立项目总纲、设计审视和可追踪开发执行清单。
- 建立 Git 忽略、行尾、编辑器和文档治理基线。
- 固化首选技术路线与项目完成标准。
- 新增可运行的 gateway 进程骨架、存活/就绪探针和有限时优雅关闭。
- 新增独立 control-plane 进程、基础管理状态路由，并抽取多进程共享 HTTP 生命周期。
- 新增 metering-worker 事件总线 bootstrap、broker 回退和有限时会话关闭骨架。
- 新增版本单调、校验和可验证、敏感字段受限的不可变业务配置 Snapshot 与热更新 Store。
- 新增三进程统一 JSON 结构化日志、固定关联字段、递归敏感属性脱敏与安全 Bootstrap 错误码。
- 新增统一 API 错误模型，严格隔离内部 cause 与公开响应，并统一健康、404、405 和未知 500 的安全 JSON 渲染。
- 新增安全 Request ID 与 W3C Trace Context 中间件，支持冲突重生成、近期重放窗口、响应回传和跨进程 HTTP 传播。
- 新增请求/流连接注册表：关停时立即取消流、有限排空普通请求、超时强制取消，并为 Worker Session Close 增加外层 deadline 保证。
- 新增三进程真实二进制生命周期集成测试模板，并统一本地/CI 的 integration build-tag、race 与 lint 入口。
- 新增 Tenant/Project PostgreSQL 隔离根迁移，包含状态、配额引用、乐观锁、审计字段、租户内唯一约束及真实数据库约束测试。
- 新增 Virtual API Key PostgreSQL 模型：仅持久化不可逆 32 字节 keyed digest 与安全前缀，数据库强制租户/项目隔离、授权覆盖、正整数限额和轮换/吊销生命周期。
- 新增 Virtual Key 生命周期服务与管理 API：加密随机签发、一次性明文返回、HMAC-SHA-256 摘要、事务化单替代者轮换、幂等吊销和读取时派生过期，并以真实 PostgreSQL 并发测试验证。
- 新增数据面 Virtual Key 认证中间件：严格 Bearer 解析、版本化 HMAC 常量时间验证、Tenant/Project/Key 状态决策、可信 Principal Context、有界正缓存与显式失效。
- 新增模型目录：分离 Provider、租户 LogicalModel 与物理 Deployment，以严格 Capability/Region/Data Retention 契约和数据库触发器阻止不兼容绑定及后续配置漂移。
- 新增项目模型授权与 `GET /v1/models`：计算项目白名单、Key 三态收窄和 active 目录交集，并修复显式空 Key 白名单被折叠为继承策略的问题。

### Changed

- 将产品差异点聚焦到流式调用可核算、并发预算不超卖、协议适配可验证和路由决策可解释。

### Security

- 默认禁止提交本地环境文件、私钥、证书和运行数据。
- Virtual API Key Schema 不提供明文、可逆密文或原始 Key 字段，并以复合外键阻止跨租户项目和轮换链。
- 完整 Virtual Key 不进入领域 Record、数据库、查询 API、错误或日志；非开发环境缺少独立摘要密钥时控制面拒绝启动。
- Gateway 删除客户端伪造的租户/项目/Key 身份 Header；所有凭据失败统一 401，数据库或 Keyring 不完整时 fail closed 为 503。
