# Provider 规范化类型契约

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P05-T02
>
> 实现：`internal/adapter`

## 1. 目标

Provider Adapter 的困难不是字段改名，而是供应商对结束原因、流事件、缓存 Token、未知计费项和错误重试的语义不同。本契约建立一组供应商无关、可校验且可保留未知证据的事实类型，使后续路由、代理、计量和一致性测试不依赖某个供应商 SDK。

本层只回答“请求要求什么、Provider 实际返回了什么”，不负责：

- 选择 Deployment 或读取凭据；
- 发起 HTTP、重试、故障切换或熔断；
- 猜测未知字段的费用；
- 计算价格、写 Usage Ledger；
- 把内部类型直接序列化为客户端协议。

## 2. 类型关系

| 类型 | 作用 | 关键不变量 |
|---|---|---|
| `NormalizedRequest` | Adapter 的统一输入 | 无 Tenant/Project/Key/Endpoint；参数存在性不被零值折叠 |
| `NormalizedResponse` | 完整非流式结果 | Choice 唯一；Assistant Message；结束原因有限且未知值留证 |
| `NormalizedChunk` | 单个流式事实 | Sequence + Kind；不同 Kind 的 Payload 不能混用 |
| `NormalizedUsage` | 计量事实 | Missing ≠ 0；不同 Token 维度不擅自相加；未知字段留原始证据 |
| `NormalizedError` | 安全错误事实 | 有限分类；Unknown 不猜重试；没有 Raw Body 或内部 cause |

## 3. NormalizedRequest

请求包含：

- `requestId`、`logicalModel`、`messages`；
- `stream`、可选 `temperature`、`topP`、`maxOutputTokens` 和 `stop`；
- Function Tool Schema、Tool Choice、JSON Response Format；
- Tenant Policy 已产生并排序去重的内容/区域标签；
- 最多 64 KiB、顶层为 JSON Object 的 `providerOptions`。

Pointer 表示“客户端是否显式提供参数”，因此 `temperature=0` 不会与未提供混淆。Messages 支持 Text、Image Reference、Audio Reference、Assistant Tool Call 和 Tool Result，但媒体 Reference 的出站解析仍由后续 SSRF/Egress Policy 执行。

`providerOptions` 在本层只做结构和大小校验；调用方必须先按 Deployment Schema 验证。Adapter 对不能表达且会影响结果、工具或费用的字段必须返回 `unsupported_parameter`，不能静默删除。

## 4. NormalizedResponse 与结束原因

非流式响应保留 Response ID、Provider 实际模型、Choice、可选 Usage、Provider Request ID 与观察时间。统一结束原因只有：

- `stop`
- `length`
- `tool_calls`
- `content_policy`
- `cancelled`
- `error`
- `unknown`

当归类为 `unknown` 时，`providerFinishReason` 必填；这样供应商新增结束原因不会被错误转换为正常结束。已识别原因也可以保留原始值，用于协议漂移对比。

## 5. NormalizedChunk

每个 Chunk 有严格 Kind：

| Kind | 允许的主要 Payload | Usage 规则 |
|---|---|---|
| `message_start` | Role | 无 |
| `content_delta` | Content Delta | 无 |
| `reasoning_delta` | Reasoning Delta | 无，默认不等同客户端内容 |
| `tool_delta` | Tool ID/Name/Arguments Fragment | 无 |
| `usage_delta` | `complete=false` Usage | `usageStatus=partial` |
| `message_end` | Finish Reason + 可选 Usage | 必须明确 `present/partial/missing` |
| `heartbeat` | 仅事件类型元数据 | 永不进入模型内容 |
| `provider_extension` | 最多 16 KiB JSON Object | 默认隔离，不向客户端透传 |

Validation 会拒绝跨 Kind 混入 Payload，例如同时出现 Content Delta 与 Reasoning Delta。`message_end + usageStatus=missing` 明确表示供应商没有报告 Usage，不能产生一个全 0 Usage。

Sequence 的跨 Chunk 单调性属于 `ChunkStream`/一致性套件职责；单个值类型只保证它不是有符号负数。

## 6. NormalizedUsage：缺失、零与未知计量

每个计量维度使用：

```go
type TokenCount struct {
    Value   int64
    Present bool
}
```

因此：

- `{Value: 0, Present: true}`：Provider 确实报告 0；
- `{Value: 0, Present: false}`：字段缺失；
- 非 Adjustment 来源不允许负数；Adjustment 可用负数表达后续反向修正。

标准维度包括 Input、Output、Cache Read、Cache Write、Reasoning、Audio Input 和 Audio Output。Cache Read 可能是 Input 的子集，也可能是独立可计费 Meter；本层不做相加假设，价格版本负责解释。

Usage 来源是 `provider | estimated | reconciled | adjustment`，`complete` 表示是否最终完整。Provider 和 Reconciled 来源必须携带 `UsageEvidence`。

### 6.1 未知字段证据

Adapter 收到未知计量字段时：

1. 将精确 Provider Usage JSON 交给 `NewUsageEvidence`；
2. 原始对象不得超过 64 KiB；构造函数复制字节并计算精确 SHA-256；
3. 将无法映射的字段以排序、去重 JSON Pointer 写入 `UnmappedFields`；
4. Pricing/Metering 在映射明确前不得把未知项按 0 结算；
5. 普通 `json.Marshal` 和 `slog` 只输出 Evidence Hash 与大小，原文只能显式 `Bytes()` 读取。

示例：

```go
evidence, err := adapter.NewUsageEvidence(
    []byte(`{"input_tokens":13,"cache_read_tokens":5,"future_meter":2}`),
)
usage := adapter.NormalizedUsage{
    InputTokens:    adapter.Tokens(13),
    CacheReadTokens: adapter.Tokens(5),
    Source:         adapter.UsageSourceProvider,
    Complete:       true,
    RawEvidence:    evidence,
    UnmappedFields: []string{"/future_meter"},
}
```

## 7. NormalizedError

统一分类：`auth`、`permission`、`invalid_request`、`rate_limit`、`capacity`、`timeout`、`provider_5xx`、`content_policy`、`context_length`、`protocol`、`cancelled`、`unknown`。

字段只包含稳定 Code、Category、Retryable、可选 Retry-After、Provider HTTP Status、安全消息和 Provider Request ID。设计上没有 Raw Body、Prompt、Response、Credential 或内部 cause；`Error()` 也只返回 Code 和 Safe Message。

`unknown` 默认必须 `retryable=false`，避免在没有语义证据时盲目重试并放大费用。真正的重试决策仍由 Gateway 结合首包状态、Attempt 上限和总 Deadline 执行。

## 8. 可变性与安全日志

- `Clone` 深拷贝 Messages、Tool Arguments、Schema、Options、Pointers、Usage 元数据和 Provider Extension，避免并行 Route Attempt 相互修改。
- `UsageEvidence` 内部不可变，输入和输出都防御性复制。
- Request/Response/Chunk/Usage/Error 实现 `slog.LogValuer`；日志只保留 ID、Kind、数量、计量值和 Evidence Hash。
- Prompt、Response Content、Reasoning、Tool Arguments、Schema、Provider Options、Provider Extension 和 Usage 原文不会通过这些 LogValue 进入日志。

## 9. 自动化验收

`internal/adapter` 测试覆盖：

- 所有 Request/Response/Chunk/Error/Usage 正常类型与主要非法组合；
- Missing 与真实 0、负 Adjustment、Unknown Finish/Error；
- 未知 Usage 字段原字节保留、精确 SHA-256、64 KiB 上限和防御性复制；
- Clone 不共享 Slice、Pointer 或 Raw JSON；
- 结构化日志不包含 Prompt、Response、Tool Arguments、Options、Extension 或 Usage 原文；
- NormalizedError 反射检查没有 Raw Body、Cause、Credential、Prompt 或 Response 字段。

P05-T03 已基于这些类型建立不可变 Factory 注册表与启动/发布未知 Type 门禁；P05-T04 已通过真实 Mock Provider HTTP 协议验证类型转换，P05-T05 已以统一 Conformance Suite 对所有 Expected Response/Chunk/Error 再执行本文件的领域校验。
