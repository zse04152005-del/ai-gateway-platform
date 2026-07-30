# Go 依赖登记

> 新增或升级依赖时必须同步更新本文件，并通过测试、漏洞扫描和许可证检查。

## 生产构建依赖

| 模块 | 固定版本 | 许可证 | 用途 | 采用理由与边界 |
|---|---|---|---|---|
| `github.com/golang-migrate/migrate/v4` | `v4.19.1` | MIT | SQL 迁移状态、锁、up/down/version 执行 | 采用成熟迁移引擎，项目只封装命令和顺序校验；不用于业务查询，不记录数据库连接串 |
| `github.com/lib/pq` | `v1.12.3` | MIT | PostgreSQL 迁移驱动与 P04 Virtual Key 事务 Store | 复用已审计依赖完成最小 `database/sql` 生命周期事务；只使用参数化 SQL，不记录连接串；查询规模扩大时再以基准和 ADR 评估 pgx/sqlc，避免当前重复驱动 |

## 开发/CI 工具依赖

| 工具模块 | 固定版本 | 许可证 | 用途 |
|---|---|---|---|
| `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` | `v2.12.2` | GPL-3.0（工具本体；输出不受传染） | 格式、Lint、静态分析和安全规则聚合 |
| `golang.org/x/vuln/cmd/govulncheck` | `v1.6.0` | BSD-3-Clause | 基于实际可达调用链的 Go 漏洞扫描 |
| `github.com/rhysd/actionlint/cmd/actionlint` | `v1.7.12` | MIT | GitHub Actions 语法、表达式和 Shell 片段静态检查 |

两项工具通过 `go.mod` 的 `tool` 指令固定，并以 `go tool <name>` 调用。它们及其分析器依赖不会链接进项目生产二进制。

## 供应链约定

- `go.mod` 固定版本，`go.sum` 固定模块内容校验值。
- CI 必须运行 `go mod verify` 与 `govulncheck ./...`。
- 不接受用途重复的大型依赖；新增依赖需记录替代方案、许可证和风险。
- `go.sum` 可能包含依赖测试图的校验值，不代表这些模块都会链接进生产二进制；实际构建图以 `go list -deps` 为准。
