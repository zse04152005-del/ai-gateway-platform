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

### Changed

- 将产品差异点聚焦到流式调用可核算、并发预算不超卖、协议适配可验证和路由决策可解释。

### Security

- 默认禁止提交本地环境文件、私钥、证书和运行数据。
