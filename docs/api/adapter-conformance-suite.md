# Adapter Conformance Suite

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P05-T05
>
> 实现：`internal/adapterconformance`

## 1. 目标

Provider Adapter 的风险不只是“能否请求成功”，还包括流终态、Usage 证据、错误重试语义、取消传播和协议漂移。如果每个 Adapter 自己复制测试，断言会逐渐不一致，也容易用进程内假对象绕过真实 HTTP 行为。

统一套件把测试算法固定在平台侧，把供应商差异限制在一个注册对象中：

```text
Registration
  + AdapterBuilder(endpoint)
  + protocol Handler Fixtures
  + NormalizedRequest
  + expected Response/Chunk/Error
        |
        v
shared httpserver Handler -> real httptest TCP server
        |
        v
BuildRequest -> HTTP Client -> ParseResponse/OpenStream
        |
        v
shared semantic assertions
```

新增 Adapter 不得复制或删减统一断言；只需为供应商协议注册可重复的合成 Fixture。

## 2. 注册接口

核心入口：

```go
func Run(t *testing.T, registration Registration)

type Registration struct {
    Name       string
    NewAdapter AdapterBuilder
    Fixtures   FixtureSet
}

type AdapterBuilder func(
    ctx context.Context,
    endpoint string,
) (provideradapter.Adapter, error)
```

`AdapterBuilder` 应调用 Adapter 已有的 Factory/Registry 路径，把套件提供的隔离 Endpoint 绑定为测试 Deployment。它不是直接返回 Normalized 数据的回调。

每个 `HandlerFactory` 返回一个新的供应商协议 Handler。Handler 可以输出 OpenAI-compatible JSON/SSE，也可以输出其他 Adapter 自己支持的 HTTP 协议；统一套件不解析供应商正文，只调用公开 Adapter 接口。

## 3. 强制 Fixture 矩阵

`FixtureSet` 使用显式字段而非自由切片，避免空集合造成假通过：

| Fixture | 强制语义 |
|---|---|
| Ordinary | 非流式成功、`stop`、完整 Provider Usage |
| Stream | message start、content delta、message end、终态 Usage、EOF |
| Cancellation | 阻塞 `Next` 可取消，Body 关闭，上游请求 Context 收到取消 |
| RateLimit | 429 映射为 retryable `rate_limit` |
| ProviderFailure | retryable `capacity` 或 `provider_5xx` |
| CachedUsage | 正数 Cache Read Token，不与普通 Input Token 混合 |
| ToolCall | 完整 Tool Call 与 `tool_calls` finish reason |
| FinishReasons | 至少覆盖 `length`、`content_policy`、`unknown` |
| UnknownOrdinary | 普通结构漂移以注册的 Protocol Sentinel fail closed |
| UnknownStream | 未知流字段隔离为有限大小 `provider_extension` |

当前 MVP 面向同时支持 Chat、Stream、Tools 和 Cache Usage 的首批 Adapter，因此上述项目不可声明 Skip。后续若引入不具备某项 Provider 能力的 Adapter，必须先扩展“Capability 与 Not Applicable 原因”契约，不能直接传空 Fixture 或删除测试。

## 4. 注册前门禁

`Registration.Validate` 在创建 Listener 前验证：

- Registration 与 Fixture Name 是规范、唯一的标识；
- Builder/Handler/Error Sentinel 均非 nil；
- 所有 Request、Expected Response/Chunk/Error 通过领域 `Validate`；
- 普通 Fixture 的 Usage 来源是 Provider 且完整；
- Cache、Tool、Finish Reason 和 Stream Chunk 类型覆盖完整；
- Chunk Sequence 从 0 单调递增；
- Unknown Stream 确实产生 `provider_extension`；
- 429 与 Provider Failure 的 Category/Retryable 组合安全；
- 合成敏感标记非空且规范。

失败统一满足 `errors.Is(err, ErrInvalidRegistration)`，测试不启动，也不会部分执行。

## 5. 真实 HTTP 隔离

每个 Case 都会：

1. 创建新的 Handler；
2. 包装项目共享 `httpserver` 横切边界；
3. 启动真实 `httptest.Server`；
4. 使用该 Server Origin 新建 Deployment-scoped Adapter；
5. 调用 `BuildRequest`；
6. 校验生成 URL 的 Scheme/Host 没有逃离隔离 Origin；
7. 通过 Server Client 发送真实 HTTP 请求；
8. 调用 `ParseResponse` 或 `OpenStream/Next`。

因此 Fixture 必须经过 JSON/SSE 编码、HTTP Transport、Header、Body 所有权、网络 EOF 与 Context 取消边界，不能按测试名直接返回内存结果。

## 6. 统一断言

### 6.1 Request 与 Response

- `BuildRequest` 不得修改注册的 `NormalizedRequest`；
- 不允许返回 nil Request/Adapter 而没有 Error；
- NormalizedResponse 必须再次通过领域校验且具有非零 `ObservedAt`；
- Message、Tool Call、Finish Reason、Usage Evidence 与 Unmapped Fields 按期望比较；
- 比较时只忽略 Wall Clock 的具体时间，并把 nil/empty slice 视为同一领域语义。

### 6.2 Stream

- 每个 Chunk 都通过领域校验；
- Sequence 必须与读取顺序完全一致；
- 读到 Provider `[DONE]` 或对应终态后返回 `io.EOF`；
- 再次调用 `Next` 必须继续返回 `io.EOF`，不能产生幽灵 Chunk；
- Unknown Event 只进入 `provider_extension`，不混入 Content。

### 6.3 Cancellation

取消一个正在阻塞的 `Next(ctx)` 后：

- `Next` 必须在 5 秒硬上限内返回 `context.Canceled`；
- Adapter 必须关闭 Body，使 HTTP Transport 取消上游 Request Context；
- 已关闭 Stream 不能再返回成功 Chunk。

### 6.4 Error Safety

Error Fixture 通过普通 `ParseResponse` 入口执行，不直接调用内部分类函数。返回值必须可 `errors.As` 为 `NormalizedError`、通过领域校验并完全匹配预期 Category/Retryable/Retry-After/Status。

Fixture 可把 Provider 原始 Message 或合成凭据片段注册为 `ForbiddenText`；统一套件同时检查外层 Error String 与 `NormalizedError.Error()`，防止原始正文泄漏。

## 7. 新增 Adapter 流程

1. 在 Adapter 测试包中实现一个 `Registration` 构造函数；
2. Builder 使用真实 Factory/Registry 和套件 Endpoint；
3. 为每个显式 Fixture 提供供应商协议响应；
4. Expected Usage 必须包含与响应正文一致的 Raw Evidence；
5. 调用 `adapterconformance.Run(t, registration)`；
6. 本地至少运行 20 轮稳定测试；
7. Linux CI 使用 race detector 重跑全套；
8. 记录 Provider 协议版本和 Fixture 来源，不保存真实 Key、Prompt 或 Response。

Mock Adapter 的完整示例位于 `internal/mockadapter/conformance_test.go`。它使用 `mockprovider.NewHandler` 覆盖普通/SSE/429/503/缓存/工具场景，并以额外合成 Handler 覆盖 Finish Reason 与未知字段。

## 8. 当前验证

- `adapterconformance` 自身使用一个最小脚本协议 Adapter 验证运行器、真实 HTTP、错误、Stream 与取消控制流；
- Registration 非法矩阵覆盖缺失 Builder/Handler、重复名、非法 Request/Expected Fact、Sequence、Capability 语义和敏感标记；
- Mock Adapter 通过全部统一 Fixture，同时保留 P05-T04 的协议边界专项测试；
- 统一框架和 Mock Adapter 均执行 20 轮重复测试，Linux CI 继续强制 race detector。
