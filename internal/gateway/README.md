# gateway

数据面应用路由装配层。共享 `httpserver` 负责健康、关联与连接生命周期；本模块负责把所有 `/v1/*` 路由放在 `keyauth.Authenticator` 之后。

`GET /v1/models` 已实现项目白名单、Key 收窄策略与目录可用性过滤，只返回稳定逻辑模型及客户端能力；不返回 Provider、物理模型、Deployment、Endpoint 或其他租户信息。目录不可可信时 fail closed 为 503 `MODEL_CATALOG_UNAVAILABLE`。

`POST /v1/chat/completions` 的非流式路径已接通严格解析、规范化、可信选路、单次 Provider 执行与统一 JSON。公共 `model` 始终是逻辑模型；Usage 不把缺失计量伪造为 0，Tool Call Arguments 保持 JSON 字符串，Provider Body/Endpoint/物理模型和私有错误不进入响应。`stream=true` 在 P07 前明确返回 501，不静默降级。

P08-T03 用 `ObservedChatExecutor` 包装真实非流式 Attempt，并把终态分类为成功、429、5xx、Provider Timeout、Caller Cancellation 或 Other Failure，连同总延迟写入进程级 `routing.PassiveHealth`。记录使用 `context.WithoutCancel`，因此客户端取消后仍可完成本地统计；观察失败只增加本地 Failure Counter，不得篡改已经产生的 Provider Response/Error。非流式路径不把完整响应延迟冒充 TTFT，真实 First Token 指标由流式 Attempt 在获得首模型事件时写入同一 Observation Contract。

P08-T05 的 `CircuitChatExecutor` 位于 `ObservedChatExecutor` 外层，在真正调用 Provider 前原子获取 Deployment Circuit Permit。Open 或 Half-Open 并发已满时不会调用下层，也不会污染被动健康；公开响应统一为 503 `MODEL_UNAVAILABLE`，不暴露熔断状态。已获得 Permit 的真实结果按有限分类完成：成功推进恢复，429/临时容量/Timeout/5xx/协议或 Transport 故障计为失败，Caller Cancellation、认证、权限、参数、上下文和本地 Adapter 配置故障只释放 Permit 而不惩罚 Provider。完成记录失败只增加本地 Counter，不篡改业务响应。

P08-T07 为非流式生产请求增加有界故障切换：默认最多 3 个物理 Attempt、30 秒统一总时限和 250ms 下一 Attempt 最小窗口。失败 Attempt 先独立落库并保持父 Request running，再排除已尝试 Deployment 重选；普通 retry 无备用时才允许同一固定目标，different-only 永不回退旧目标。公共响应在最终投影和终态落库前不写字节，`gateway.attempt_count` 返回真实尝试数；所有 Attempt 的已知 Usage 独立保留给 P10 聚合。

P10-T07 在每次选定 Deployment 后提供本地 Usage fallback。Provider Usage 只要存在就保持原样且优先；仅在完全缺失时调用 `tokenestimate`。成功响应可估算 Input/Output，已提交但响应不可验证的失败只保留 Input 估算；未提交失败不制造费用。所有本地结果固定为 `source=estimated`、携带版本化 tokenizer/model 证据，Gateway 会拒绝任何试图返回 `source=provider` 的本地估算器。

P08-T08 在每次 Selector 评估后、创建 Attempt 前追加无内容 RouteDecision，并在每个失败 Attempt 结束前追加 RetryDecision。初选、无候选、排除旧目标、固定目标复用和最终 `no_retry` 都可形成 requestId 因果链。任一必需决策记录失败都 fail closed：初选记录失败不调用 Provider，retry 记录失败则终结当前 Attempt 且不创建下一 Attempt。公开诊断 API 仍由 P16-T06 实现。

P08-T09 用 64 路并发全故障回归锁定线性放大上界：默认三次 Attempt 时精确产生 `3N` 个 Provider 调用和 Attempt，单 requestId 始终串行。推进时钟证明共享总 Deadline 不会按 Attempt 重置；额外费用被拒绝时只保留首个物理调用及其完整计量/决策事实。

其他未实现业务端点返回安全 JSON 404。非 `/v1/*` 未知路径不触发数据库认证，避免扫描流量消耗认证资源。
