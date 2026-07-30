# Runbook

MVP 前至少提供以下运行手册：

- Provider 大量 429/5xx。
- SSE 连接或 Goroutine 泄漏。
- 预算预留无法释放。
- 用量事件积压或账本缺失。
- 配置发布失败或实例版本不一致。
- ClickHouse/分析链路故障。

Runbook 必须包含症状、确认指标、影响、立即操作、恢复验证和后续行动。

已实现的运行手册：

- [`graceful-shutdown.md`](graceful-shutdown.md)：三进程关闭、普通请求排空、流取消和强制截止时间。
