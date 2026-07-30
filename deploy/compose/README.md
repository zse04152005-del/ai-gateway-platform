# 本地 Compose 环境

## 前置条件

- Docker Desktop/Engine 与 Compose v2。
- 根目录 `.env`（从 `.env.example` 复制，仅用于本地）。

## 服务

| 服务 | 默认端口 | 用途 |
|---|---:|---|
| PostgreSQL | 5432 | 事务事实源 |
| Redis | 6379 | 限流、并发和配置缓存 |
| Redpanda | 19092 | Kafka API 事件总线 |
| ClickHouse HTTP | 8123 | 分析查询 |
| OTel gRPC/HTTP | 4317/4318 | 遥测接收 |
| Prometheus | 9090 | 指标 |
| Grafana | 3000 | Dashboard |

## 持久化卷

| 卷 | 挂载位置 | 保存内容 |
|---|---|---|
| `postgres-data` | PostgreSQL `/var/lib/postgresql/data` | 租户、配置、账本等事务数据 |
| `redis-data` | Redis `/data` | AOF 与本地缓存状态 |
| `redpanda-data` | Redpanda `/var/lib/redpanda/data` | Kafka 日志与元数据 |
| `clickhouse-data` | ClickHouse `/var/lib/clickhouse` | 分析事件与聚合数据 |
| `prometheus-data` | Prometheus `/prometheus` | 指标时序数据 |
| `grafana-data` | Grafana `/var/lib/grafana` | Dashboard、用户和插件状态 |

执行普通 `docker compose down` 不会删除这些命名卷。只有明确需要清空全部本地基础设施数据时才可执行 `docker compose down --volumes`；该操作不可通过 Compose 恢复，应先确认不再需要本地数据。

## 验证命令

在项目根目录执行：

```powershell
docker compose --env-file .env -f deploy/compose/compose.yaml config --quiet
docker compose --env-file .env -f deploy/compose/compose.yaml up -d --wait --wait-timeout 180
docker compose --env-file .env -f deploy/compose/compose.yaml ps
```

2026-07-30 已在 Docker Desktop 4.84.0、Docker Engine 29.6.2、Compose v5.3.1、WSL 2.7.11.0 环境真实完成镜像拉取和全服务健康检查。ClickHouse 探针显式使用 `127.0.0.1`，避免 BusyBox `wget` 将 `localhost` 解析为未监听的 IPv6 `::1`。
