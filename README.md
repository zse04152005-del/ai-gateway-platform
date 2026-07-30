# AI Gateway Platform

企业级 AI 网关与模型成本治理平台项目目录。

当前处于：**本地基础设施与工程工具阶段（P02）**。P00/P01 已完成；Go、Docker/WSL2 与 7 个本地基础服务已经真实验证。

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

`P02 阶段门禁：首次远程 GitHub Actions 运行`

本地 P02 任务全部完成。当前缺少 Git `user.name`/`user.email` 和远端仓库，不能创建真实身份提交或触发远程 CI；在获得这些信息并看到 Actions 通过前，不进入 P03。

开始实现前先打开《开发执行清单.md》，把当前任务从 `[ ]` 改为 `[~]`；完成并验证后改为 `[x]`，填写日期和证据。

## 计划技术栈

- Go 1.26.5（当前已安装版本；`go.mod` 最低工具链为 1.26.0）
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

`.env` 只用于本地并被 Git 忽略，禁止写入真实生产密钥。
