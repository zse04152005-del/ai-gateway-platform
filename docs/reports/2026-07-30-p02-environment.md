# P02 工程环境阶段性检查

> 日期：2026-07-30  
> 状态：进行中

## 已验证

- Git：2.51.2.windows.1。
- Go：1.26.5 windows/amd64；安装包来自 Go 官方，SHA-256 与官方元数据一致。
- `go.mod`：模块 `github.com/aigateway-lab/ai-gateway-platform`，最低安全工具链 Go 1.26.5。
- `go mod tidy` 与 `go list -m -json` 成功。
- `go vet ./...` 成功。
- `go test -count=1 ./...` 成功。
- 配置包覆盖率：83.0%。
- OpenAPI 与所有 YAML 文件执行静态解析检查。
- 桌面项目与工作副本逐文件 SHA-256 校验一致；Go 测试也已在桌面项目目录运行通过。
- Docker Desktop 4.84.0、Docker Engine 29.6.2、Compose v5.3.1 已安装；引擎为 Linux/x86_64、Cgroup v2，运行于 WSL2。
- Compose 真实解析与 7 个固定版本镜像拉取成功；PostgreSQL、Redis、Redpanda、ClickHouse、OpenTelemetry Collector、Prometheus、Grafana 全部通过 Docker 健康检查。
- 服务级烟雾测试通过：PostgreSQL `SELECT 1`、Redis `PONG`、Redpanda 集群 Healthy、ClickHouse `SELECT 1`、Prometheus Ready、Grafana database=ok、OTel health endpoint 可用、Prometheus 的 OTel Target 为 up。
- `.env` 只含本地开发默认值，桌面与工作副本 SHA-256 一致，并由 `.gitignore` 明确忽略。
- 项目内迁移命令、文件顺序校验和单元测试通过；golang-migrate v4.19.1 与 lib/pq v1.12.3 已固定并登记 MIT 许可证。
- 唯一临时 PostgreSQL 库真实通过空库 up、重复 up/no-change、version、生产 down 保护、开发 down 和再次 up；最终状态 `2:1:false`，临时库随后删除。PowerShell 一键入口已将同一基线应用到主开发库并验证版本 1、dirty=false。
- `go.mod` tool 指令固定 golangci-lint v2.12.2、govulncheck v1.6.0、actionlint v1.7.12；统一 PowerShell `check` 已真实通过模块校验、格式、vet、Lint、单测、构建、漏洞、迁移、工作流和本地高风险密钥模式扫描。
- `.golangci.yml` v2 配置校验通过，golangci-lint 输出 `0 issues`，govulncheck 输出 `No vulnerabilities found`，actionlint 退出码 0。
- CI 工作流包含 Go 质量/构建、临时 PostgreSQL 迁移状态机、YAML/禁止本地密钥位置/高风险模式/Gitleaks 三类 Job。故意未格式化且失败的临时测试文件分别令格式门禁和单测退出 1，删除后完整正向门禁重新通过。
- 本地第三方 gitleaks Docker 扫描因会向镜像暴露仓库内容而被安全策略拒绝，镜像未拉取、容器未运行；改用受控本地模式扫描，完整 Gitleaks 保留在隔离 GitHub runner。

## Docker/WSL 准备记录

- 用户已明确授权启用 WSL2、VirtualMachinePlatform、必要时重启并安装 Docker Desktop。
- 系统内核版本为 `10.0.22621.4317`，amd64；`wsl.exe` 存在。
- 首次执行 WSL 可选功能启用时，DISM 返回 `0x80070032`（进程退出码 50）。DISM 日志同时确认该功能已处于 `Staged`，失败原因是父功能尚未完成启用，而不是功能名称不存在。
- 当前检测到 `CBS RebootPending=True` 和 `PendingFileRenameOperations=True`。为避免在未完成的 CBS 事务上重复修改系统功能，下一步先重启，再核验 WSL 与 VirtualMachinePlatform 的最终状态。
- 重启后的续跑顺序：检查功能状态 -> 启用仍未启用的组件 -> 再次重启（若系统要求）-> 验证 `wsl --status` -> 下载并校验 Docker 官方签名 -> 安装 Docker Desktop -> 验证 Compose 与 7 个基础服务。
- 第一次重启后复核结果：`Microsoft-Windows-Subsystem-Linux=Enabled`、`VirtualMachinePlatform=Disabled`，CBS 待重启标志已清除；CPU 报告 `VirtualizationFirmwareEnabled=True`、`VMMonitorModeExtensions=True`、`SecondLevelAddressTranslationExtensions=True`。
- 已成功启用 `VirtualMachinePlatform`，即时查询状态为 `Enabled`；Windows 返回 `RestartNeeded=True`，并再次产生 `CBS RebootPending=True`。下一步执行第二次必要重启，之后不重复修改已经启用的功能。
- 第二次重启后确认：WSL 与 VirtualMachinePlatform 均为 Enabled，`HypervisorPresent=True`，CBS/WU/PendingFileRenameOperations 均无待重启项；无需启用完整 Hyper-V。
- WSL Store 更新通道执行成功，当前为 WSL 2.7.11.0、内核 6.18.33.2，默认 WSL 版本为 2。Web 下载通道曾返回 403，没有造成系统改动，随后使用 Store 通道完成。
- Docker Desktop 官方 amd64 安装器下载大小 613.4 MB，SHA-256 为 `FE54164C1CEB9E2004137E22E4013826BACCF2352C1CEDB27E8DAA8E56230DD7`；Authenticode 状态为 Valid，签名者为 Docker Inc，安装器版本 4.84.0.234817。
- Docker Desktop 使用 `--backend=wsl-2` 安装成功，退出码 0；已验证 Docker CLI 29.6.2、Compose v5.3.1、`com.docker.service=Running`，当前用户已写入本机 `docker-users` 组。
- 安装后系统产生 CBS 待重启项（PackagesPending 有 204 个子项），当前登录令牌尚未包含 `docker-users`。下一步重启完成组件事务并刷新登录令牌，然后首次启动 Docker Desktop 和验证引擎。
- 安装收尾重启后，CBS/WU/PendingFileRenameOperations 均为 False，当前登录令牌已包含 `docker-users`；Docker Linux 引擎启动成功。
- 首次 Compose 等待定位到 ClickHouse 健康检查失败：BusyBox `wget` 将 `localhost` 解析为未监听的 IPv6 `::1`。改为 `127.0.0.1` 后只重建 ClickHouse，7 个服务全部变为 healthy。

## 尚未验证

- Windows 本机 `-race` 需要 CGO/C 编译器；当前本地环境未安装，CI 的 Linux Runner 将执行 race test。
- GitHub Actions 尚未由远程仓库触发。
- Git 仓库尚未配置 user.name/user.email，因此未创建初始提交，避免伪造用户身份。

以上未验证项保持清单未完成，不作为通过证据。
