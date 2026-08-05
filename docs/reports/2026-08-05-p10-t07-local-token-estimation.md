# P10-T07 本地 Token 估算接口验收报告

- 日期：2026-08-05
- 范围：模型绑定的本地 Token 估算、TPM 身份复用、UsageEvent v2、Outbox/Ledger 证据持久化
- 结论：实现、本机 Migration 演练、完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

实现提交 `0f1dd3d` 新增 `internal/tokenestimate`。默认 `utf8-byte-bound/v1` 对内容相关的规范化 JSON envelope 进行确定性 UTF-8 byte 上界计数；它明确是 Gateway 本地估算，不声称复刻供应商 BPE 或产生供应商精确计费值。算法可通过 `Tokenizer` 接口替换，但名称与版本必须随语义变化。

每份估算都携带 `estimated=true`、tokenizer 名称与版本、Catalog `physical_model`、Deployment version 和 provider protocol version。估算器还可通过 `BindInput` 实现 TPM 的 `InputTokenEstimator`，因此准入预留和缺失 Usage 时的计量回退共享同一组 tokenizer/model 身份。

## 2. Gateway 来源优先级

Provider 返回任意合法 Normalized Usage 时保持其原始来源，本地估算器不运行；只有 Provider 完全没有 Usage 时才允许回退。成功响应估算 Input 与 Output，物理请求已提交但没有可验证响应时只估算 Input，`not_submitted` 路径不制造 Usage。估算失败保持未知，不伪造零值，也不覆盖供应商结果。

`NormalizedUsage` 只有 `source=estimated` 才能携带估算证据；Provider、Reconciled 或 Adjustment 携带该证据会 fail closed。本地估算器返回 Provider 来源、完整值或缺失元数据时同样被 Gateway 拒绝，不能冒充供应商精确事实。

## 3. 内容与缓存边界

Input envelope 只包含消息、工具定义、采样参数、Stop、响应格式和 Provider Options；Output envelope 只包含规范化 Choices。Request ID、Tenant/Project、Credential、Endpoint 和 Policy Label 不进入计数内容。任何超过 Deployment Context/Output Capability 或 `2^53-1` 的结果均拒绝。

进程内并发 LRU 默认上限 4096 项，键为 tokenizer/model 身份、方向和 canonical envelope 的 SHA-256，值仅保存整数计数。缓存不保留 Prompt、Response 或工具正文，也不在统计和日志中暴露摘要。

## 4. UsageEvent 与持久化

当前生产 UsageEvent 升级为 Schema v2，Topic 继续使用 `ai-gateway.usage.v1` 并兼容历史 v1。v2 estimated 事件必须携带完整 tokenizer/model 证据；v1 禁止突然携带 v2 字段，v2 Provider 事件也禁止携带估算证据。

Migration 19 在 Usage Event Outbox 和 Usage Ledger 保存估算证据，并在 Ledger 保存 `event_schema_version`。数据库约束对每个估算字段显式检查 `IS NOT NULL`；Outbox 生成还核对证据与所选 Deployment 的物理模型、目录版本和协议版本。估算证据不可更新，历史 Provider 行继续保持 `NULL`。

## 5. 本机门禁

- `internal/tokenestimate` 专项单元测试连续 20 轮通过；真实 PostgreSQL/Redpanda 三个估算计量链用例连续 20 轮通过；
- 完整 integration 通过；Migration 恢复后首次回归遇到一次 Redis RPM 并发用例 `i/o timeout`，确认全部容器 healthy 后原命令重跑通过；
- `scripts/dev.ps1 -Action check` 通过模块校验、格式、双标签 lint、全量单测/构建、govulncheck、19 个迁移顺序、Actions 语法和本地密钥扫描；
- 覆盖率：`tokenestimate` 82.0%、Gateway 81.0%、Adapter 89.3%、Metering 93.8%；
- Down 前确认 Usage Event Outbox、Usage Event Receipt 与 Usage Ledger 均为 0 行；使用最终迁移文件完成 `19→18→19`，随后确认 `migration version=19 dirty=false` 并通过完整恢复回归。

## 6. 远端证据

GitHub Actions [`30988270051`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30988270051) 三个 Job 全绿：`go-quality` 通过 Linux race、进程生命周期、lint、构建与漏洞门禁；`migration-integration` 完成 Migration 19 生命周期和真实 PostgreSQL/Redis/Redpanda 集成验证；`config-and-secrets` 通过 YAML 与双重密钥扫描。
