# 本地 Token 估算与版本证据

> 对应任务：P10-T07

## 1. 目标与非目标

当供应商没有返回 Usage 时，Gateway 需要一个稳定、可审计的本地回退，避免把“未知”静默当作零成本。P10-T07 提供 `internal/tokenestimate`，但不声称复刻任何供应商私有计费 Tokenizer，也不把估算重新标为 Provider Usage。

默认算法是 `utf8-byte-bound/v1`：它把内容相关的规范化 JSON framing envelope 的每个 UTF-8 byte 计为一个本地 Token。该算法确定、保守、跨平台且容易复现，但它不是 OpenAI、Anthropic 或其他供应商 BPE 的精确实现。后续可以通过 `Tokenizer` 接口替换算法；替换必须使用新版本身份，不能在原版本下改变结果。

## 2. 模型绑定证据

每份 `UsageEstimateMetadata` 同时保存：

- `estimated=true`；
- tokenizer 名称与 tokenizer version；
- Catalog `physical_model`；
- Catalog Deployment version；
- Deployment capability 的 provider protocol version。

Normalized Usage 只有 `source=estimated` 才能携带这组字段；Provider、Reconciled 和 Adjustment 来源携带会验证失败。`complete` 只描述该来源的维度完整性，不能改变 estimated 的来源语义。

同一 Estimator 可通过 `BindInput` 固定到一个 Deployment 并实现 `limits.InputTokenEstimator`，因此 TPM 预留与计费回退共享 tokenizer/model 身份，不会各自维护一套版本字符串。

## 3. 内容边界与缓存

Input envelope 包含消息、工具定义、采样参数、Stop、响应格式和 Provider Options；不包含 Request ID、Tenant/Project、Credential、Endpoint 或 Policy Label。Output envelope 只包含规范化 Choices。输入和输出分别计数，任何超出 Deployment Context/Output Capability 或 `2^53-1` 的结果 fail closed。

进程内 LRU 默认最多 4096 项，键为 tokenizer/model identity、方向和 canonical envelope 的 SHA-256；Value 只有整数计数。缓存不保存 Prompt、Response 或工具正文，也不在日志或统计中暴露摘要。互斥保护使它可安全并发使用，淘汰只影响性能，不改变结果。

## 4. Gateway 来源优先级

非流式 Attempt 的顺序固定为：

1. Provider 返回任意 Normalized Usage 时保持原来源和证据，本地估算器不运行；
2. Provider 完全没有 Usage 且响应有效时，本地估算 Input 与 Output；
3. 物理请求已提交但响应不可验证时，只估算已知 Request Input；
4. Adapter 构造失败等 `not_submitted` 路径不制造 Usage；
5. 本地估算失败不覆盖 Provider 结果，也不把未知伪装成零。

公共 Chat 响应在 Input/Output 都存在时可以返回 `usage.source=estimated`，同时 `gateway.usage_complete=false`；内部 Attempt、Outbox 与 Ledger 保留完整估算证据。

## 5. UsageEvent 与持久化兼容

Migration 19 增加 UsageEvent Schema v2 估算字段。新 v2 estimated 事件缺少任一证据会在 Adapter、Event、Outbox 或 Ledger 边界失败；v2 Provider 事件携带估算字段同样失败。Outbox 生成还核对 metadata 与事件创建时选定的 Deployment 物理模型、目录版本和协议版本。

Topic 仍为 `ai-gateway.usage.v1`，Consumer 同时接受历史 Schema v1 和当前 v2：v1 不允许突然携带 v2 字段，v2 estimated 必须完整。Ledger 保存 `event_schema_version` 和同一 metadata，使历史估算费用能够解释；Provider Ledger 字段保持 `NULL`。
