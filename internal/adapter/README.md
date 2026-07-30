# adapter

`adapter` 是所有供应商适配器共享的纯领域协议层，不依赖 HTTP、数据库、目录 Store、供应商 SDK、密钥或计费实现。

核心边界：

- `NormalizedRequest` 只携带逻辑模型、消息、采样参数、工具、响应格式、策略标签和已由 Deployment Schema 校验的扩展；不携带 Tenant/Project、虚拟 Key、Provider Key 或 Endpoint。
- `NormalizedResponse` 与 `NormalizedChunk` 使用有限枚举表达消息、推理、工具、Usage、结束和未知 Provider Event；未知结束原因必须同时保存原始原因。
- `NormalizedUsage` 以 `TokenCount{Value, Present}` 区分“供应商报告了 0”和“字段缺失”，缓存、推理、音频维度互相独立，不在本层猜测是否相加。
- Provider/Reconciled Usage 必须带最多 64 KiB 的精确原始 JSON 证据；`UnmappedFields` 列出未映射 JSON Pointer，未知费用不得默认为 0。
- `UsageEvidence` 构造时复制原始字节并计算 SHA-256；普通 JSON 序列化和 `slog` 只输出 Hash/大小，读取原文必须显式调用 `Bytes()`，且返回防御性副本。
- `NormalizedError` 只包含安全错误码、有限分类、重试事实、上游状态和安全消息，类型中没有 Raw Body、Prompt、Response、Credential 或内部 cause。
- 每个聚合都提供 `Validate`；包含 Slice、Pointer 或 Raw JSON 的聚合提供 `Clone`，避免路由尝试、缓存或并行适配器之间共享可变内存。
- 所有核心类型实现安全 `slog.LogValuer`，日志不包含消息正文、推理、工具参数、Schema、Provider Options 或未知原始事件。

这些类型只定义事实，不负责：选择 Adapter、校验 Deployment 的供应商扩展 Schema、发 HTTP、重试、价格计算或持久化。P05-T03～T05 将在此包边界之上实现注册表、Mock Adapter 与统一一致性套件。
