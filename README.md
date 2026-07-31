# AI Gateway Platform

企业级 AI 网关与模型成本治理平台项目目录。

当前处于：**多部署治理阶段（P08）**。P00～P07 已完成；P08 已接通可解释候选过滤、固定/优先级/加权策略、被动与主动健康、Deployment 熔断、有限重试分类，以及首次客户端输出前的有界多 Attempt 故障切换。

## 项目核心特色

- 流式请求的部分成功、取消、多次上游尝试和费用均可核算。
- 使用原子预算预留与结算，避免并发请求绕过硬预算。
- Provider Adapter 必须通过统一协议一致性测试。
- 路由决策记录候选、过滤原因、质量、延迟、成本和策略版本。
- 后续扩展 MCP/A2A 工具治理与自托管推理感知路由。

## 文档索引

1. [项目开发总纲](./项目开发总纲.md)  
   完整需求、架构、数据、API、安全、测试、部署、市场痛点和优化后的优先级。

2. [项目审视与创新补充](./项目审视与创新补充.md)  
   记录本次二次评估、差异化方向、MVP 取舍和关键设计原则。

3. [开发执行清单](./开发执行清单.md)  
   后续开发的唯一进度来源。按 P00～P21 顺序执行，每完成一项立即更新状态和证据。

## 下一项任务

`P09-T01：定义 RPM/TPM/并发策略 Schema`

数据面已交付认证后的严格 Chat 解析、无损规范化、Tenant/Project/Key 作用域选路、进程级安全 HTTP 连接池、真实 `mock`/`openai` Adapter 执行、数据库强约束的 Request/Attempt 生命周期和客户端取消传播。P08 已完成最多 3 次、统一 30 秒 Deadline 的有界故障切换：每次物理调用都有独立 Attempt 和 Usage 事实，每次候选过滤、策略评分、重选因果和终止型 retry 分类均可按 requestId 复盘；64 路并发全故障验证调用量严格为 `3N`、同请求始终串行，Deadline 与额外费用门禁不会被重试绕过。

开始实现前先打开《开发执行清单.md》，把当前任务从 `[ ]` 改为 `[~]`；完成并验证后改为 `[x]`，填写日期和证据。

## 计划技术栈

- Go 1.26.5（当前已安装版本；`go.mod` 最低安全工具链为 1.26.5）
- PostgreSQL + pgx/sqlc
- Redis + Lua
- Redpanda/Kafka
- ClickHouse
- OpenTelemetry + Prometheus + Grafana
- Docker Compose
- Kubernetes + Helm/Kustomize（P1）

技术栈基线已在 P00/P01 阶段通过 ADR 固化，升级必须同步更新架构文档和回归证据。

## 本地开发入口

当前 Go 与 Docker Desktop 已安装。所有命令都从项目根目录执行：

```powershell
# 检查 Git、Go、Docker
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action check-env

# 下载/整理 Go 模块
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action bootstrap

# 运行 Go 测试
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action test

# 只读格式门禁、Lint、单测、构建、漏洞、迁移、CI 和高风险密钥模式检查
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action check

# 安装了支持 CGO 的 C 工具链后运行 race；Linux CI 强制运行
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action test-race
```

首次启动本地依赖与数据库基线：

```powershell
Copy-Item .env.example .env
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action compose-up
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action migrate-validate
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action migrate-up
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action migrate-version
```

启动 gateway 并检查健康探针：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action gateway

# 另开一个终端
Invoke-RestMethod http://localhost:8080/health/live
Invoke-RestMethod http://localhost:8080/health/ready
```

`Ctrl+C` 会先关闭 readiness，再在 `SHUTDOWN_TIMEOUT` 内排空普通在途请求。详细生命周期语义见 [`cmd/gateway/README.md`](./cmd/gateway/README.md)。

另开终端启动独立 control-plane 管理面：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action control-plane
Invoke-RestMethod http://localhost:8081/admin/v1/status
```

control-plane 与 gateway 使用不同监听端口；除非敏感状态路由外，当前已开放租户/项目作用域内的 Virtual Key 创建、查询、轮换和吊销 API。完整凭据只在创建/轮换响应出现一次；生产暴露前仍必须完成管理面 OIDC/RBAC。

启动 metering-worker 事件总线连接骨架：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action metering-worker
```

worker 当前验证 broker bootstrap 连接与优雅停止；Kafka 消费协议、用量事件和账本在 P09/P10 实现。

启动本地 Mock Provider（不需要数据库或外网）：

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action mock-provider
Invoke-RestMethod http://127.0.0.1:18082/health/ready
```

场景 ID、SSE、Usage、Tool Call、错误和断流用法见 [`docs/development/mock-provider.md`](./docs/development/mock-provider.md)。该进程只允许 development/test 和 Loopback 地址，不能作为生产 Provider。

Adapter 一致性注册、强制 Fixture 矩阵与新增 Provider 接入流程见 [`docs/api/adapter-conformance-suite.md`](./docs/api/adapter-conformance-suite.md)。

协议漂移 Finding、最小成本限制、内容安全结果和后续周期调度约束见 [`docs/api/protocol-canary.md`](./docs/api/protocol-canary.md)。

`.env` 只用于本地并被 Git 忽略，禁止写入真实生产密钥。
