# 技术栈基线

> 状态：Accepted for MVP  
> 日期：2026-07-30  
> 复审点：完成 P03 应用骨架、P07 流式代理和 P13 MVP 验收时。

## 1. 决策摘要

| 层次 | 选择 | MVP 版本策略 |
|---|---|---|
| 主要语言 | Go | `go.mod` 与本机均固定 Go 1.26.5，规避旧补丁版本标准库漏洞 |
| HTTP | 标准 `net/http` + chi | SSE/取消核心保持标准库可见性 |
| 数据库 | PostgreSQL | 固定主版本，迁移向后兼容 |
| 数据访问 | pgx + sqlc | SQL 显式、生成代码可审查 |
| 缓存/限流 | Redis + Lua | 只用于可重建或原子热状态 |
| 事件总线 | Redpanda（Kafka API） | 本地轻量，生产语义对齐 Kafka |
| 分析存储 | ClickHouse | 不进入同步请求热路径 |
| 遥测 | OpenTelemetry + Prometheus + Grafana | 禁止正文和密钥进入遥测 |
| 本地依赖 | Docker Compose | 固定镜像 digest/版本 |
| 部署演示 | Kubernetes + Helm/Kustomize | P1 实施 |

## 2. 为什么选择 Go

- `context.Context` 便于将客户端取消传播到上游模型请求。
- 标准库可直接控制连接池、Header 超时、流式读取、Flush 和优雅关闭。
- Goroutine 适合大量 SSE 长连接，但仍需有界队列、背压和泄漏测试。
- 单二进制便于构建最小容器镜像。

本项目不以框架功能数量为目标。数据面优先使用标准库和薄路由层，使连接、超时和重试语义保持可观察。

## 3. 三进程而非早期微服务

MVP 同仓库构建三个进程：

1. `gateway`：认证、策略、路由、代理、流式和用量事件。
2. `control-plane`：租户、项目、Key、模型、价格和策略管理。
3. `metering-worker`：用量幂等消费、账本和分析写入。

只有出现独立扩容、故障隔离、数据边界或团队所有权需求时才拆服务。

## 4. 数据职责

- PostgreSQL：配置、租户、Key、预算预留、Request/Attempt/Usage Ledger 的事实源。
- Redis：限流、并发槽位、配置快照缓存和通知；数据必须可恢复或有回收策略。
- Redpanda/Kafka：用量、审计和异步分析事件，采用至少一次投递与幂等消费。
- ClickHouse：调用明细和聚合分析；不可作为请求与预算的事实源。

## 5. 版本固化规则

- Go、数据库、Redis、Redpanda、ClickHouse 和遥测组件都在实际安装/Compose 创建时固定具体版本。
- 不使用 `latest` 镜像。
- 依赖升级必须通过协议、流式、预算、计量和性能回归。
- P02 开始前记录本机是否具备 Go 和 Docker；缺失时先完成获批安装。

## 6. 当前环境检查

2026-07-30 检查结果：

- Git 2.51.2 可用。
- Go 1.26.5 已从官方安装包安装，并校验官方 SHA-256。
- Docker 未安装或不在 PATH。

Docker 仍未安装，因此 Compose 验收保持未完成；Go 相关任务可以继续执行真实编译与测试。
