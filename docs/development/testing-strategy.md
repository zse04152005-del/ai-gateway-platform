# 测试分层与模板

> 状态：Implemented baseline
>
> 日期：2026-07-30
>
> 对应任务：P03-T09

## 1. 分层

| 层级 | 入口 | 外部依赖 | 目标 |
|---|---|---|---|
| 单元 | `go test ./...` | 无 | 领域边界、校验、状态机、错误与并发不变量 |
| Race | `go test -race ./...` | 无 | 注册表、Snapshot、连接生命周期等共享状态 |
| 进程集成 | `go test -race -tags=integration ./tests/integration/...` | 测试内依赖或本地 Compose | 真实二进制、端口、信号、协议和配置边界 |
| E2E | 后续 `tests/e2e` | Mock Provider + 基础设施 | 客户端到 Provider 的完整数据面 |
| 性能/泄漏 | 后续 `tests/performance` | 隔离环境 | SLO、内存、Goroutine、连接和取消传播 |

## 2. 命令进程验收模板

每个常驻进程都必须覆盖四类行为：

1. 启动：合法配置后进程保持运行，依赖连接或监听器真实建立。
2. 健康：HTTP 进程返回 live/ready 或状态接口；非 HTTP Worker 使用可观测连接状态/测试连接作为当前健康事实。
3. 取消：单元测试用 Context 精确验证，Linux 集成测试向真实二进制发送 SIGTERM 并要求退出码 0。
4. 配置错误：依赖连接/监听发生前失败，退出码非零，只输出统一 JSON 错误且不泄露配置值。

模板必须使用随机/临时资源、有限等待和 Cleanup。禁止固定等待“碰运气”、依赖个人 `.env`、访问真实供应商或把测试密钥写入仓库。

## 3. 新模块单测模板

- 正向、边界、无效输入和失败注入必须分开命名。
- 时间、随机源、连接器和存储通过窄接口/函数注入，测试不能依赖墙钟巧合。
- 并发测试使用信号 Channel 建立确定 happens-before，不以长 `Sleep` 代替同步。
- 对公开响应同时断言状态、Schema、关联 ID、安全 Header 和敏感值缺失。
- 核心不变量至少重复 20 轮；共享可变状态必须在 Linux race detector 下通过。
- 覆盖率用于发现空白，不以删分支或无断言测试追求数字；每次完成证据记录真实包覆盖率。

## 4. CI 一致性

常规源码和带 `integration` build tag 的模板分别执行 lint。CI 与 `make test-integration` 强制使用 race detector；PowerShell 在 Windows 无 CGO 环境自动省略 `-race`，其余 build tag、超时和包范围一致。任何 Skip 必须输出明确的平台/外部条件；不允许把失败改成 Skip。
