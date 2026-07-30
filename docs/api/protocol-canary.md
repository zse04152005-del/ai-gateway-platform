# Provider 协议金丝雀契约

> 状态：MVP Interface and Mock Implemented
>
> 日期：2026-07-30
>
> 对应任务：P05-T06
>
> 实现：`internal/protocolcanary`

## 1. 目标

供应商可能在不升级 SDK 的情况下新增 JSON 字段、Finish Reason、Usage Meter 或 Stream Event。只依赖离线 Fixture 会延迟发现真实协议变化；直接定时发送普通业务请求又会增加成本、泄露内容并把可用性故障误报为协议漂移。

协议金丝雀以一条极小的合成请求定期穿过真实 Adapter：

```text
periodic scheduler
  -> protocolcanary.Executor
  -> Registry.Build(Provider, Deployment)
  -> Adapter.BuildRequest
  -> constrained HTTP client
  -> ParseResponse / OpenStream
  -> content-free Result
```

P05-T06 实现 Executor、Runner、Probe/Baseline/Result、协议违规接口和 Mock 验证；生产调度持久化、频率/Jitter、告警聚合和真实 Provider 凭据装配由后续进程任务接入。

## 2. 调度接口

```go
type Executor interface {
    Run(ctx context.Context, probe Probe) (Result, error)
}

type AdapterBuilder interface {
    Build(
        ctx context.Context,
        provider catalog.Provider,
        deployment catalog.Deployment,
    ) (provideradapter.Adapter, error)
}

type HTTPDoer interface {
    Do(request *http.Request) (*http.Response, error)
}
```

`provideradapter.Registry` 原生满足 `AdapterBuilder`。Runner 不创建隐式后台任务；调度器负责选择到期 Probe，并只持久化通过 `Result.Validate` 的结果。

HTTP Client 必须由进程装配层注入，后续统一使用 P12 的 SSRF/Egress、DNS Pinning、连接池和响应限制 Client。Runner 不创建绕过网络策略的默认 Client。

## 3. 最小成本 Probe

`Probe.Validate` 在调用 Factory 或网络前强制：

- Provider/Deployment 领域合法、active 且归属一致；
- Request 本身通过 Normalized Request 校验；
- 只有一条 User Message 和一个 Text Part；
- 合成文本为 1～256 字节；
- `MaxOutputTokens` 必填且为 1～16；
- 禁止 Tool、Tool Choice、Structured Output 和 Policy Label；
- Stream Probe 必须由 Deployment 声明 Stream Capability；
- Timeout 为 10 ms～30 s，0 表示使用 Runner 的有界默认值；
- Baseline 明确给出 1～8 个排序且唯一的允许 Finish Reason，不能把 `unknown` 加入白名单。

Provider Options 可用于选择 Mock 场景或供应商受控的协议版本参数，但仍先经过具体 Adapter Schema。Probe、Result、日志和错误均不保存 Provider Options 内容。

## 4. 协议违规接口

Parser 不能把原始 Body 塞进金丝雀结果。`provideradapter` 新增最小安全诊断面：

```go
type ProtocolViolation interface {
    error
    ProtocolOperation() string
    ProtocolCode() string
}
```

Operation/Code 必须是稳定规范标识，例如：

```text
parse_response / unknown_response_field
parse_usage / inconsistent_total_tokens
read_stream / unexpected_eof
```

Runner 只接受有界 Token；实现若返回异常文本，会统一折叠为 `invalid_diagnostic`，不会进入结果。Mock Adapter 的安全 `ProtocolError` 已实现该接口，并继续通过 `errors.Is(ErrProtocol)` 保持专项测试兼容。

## 5. 漂移信号

| Finding | 触发条件 | 保留信息 |
|---|---|---|
| `protocol_violation` | JSON/SSE/Identity/Size/State Machine 解析失败 | 安全 Operation/Code 路径 |
| `unexpected_finish_reason` | Finish Reason 不在 Baseline | 路径 + 原始值 SHA-256 |
| `unmapped_usage_field` | Usage 包含 Adapter 未映射字段 | 已校验 JSON Pointer |
| `provider_extension` | Stream 隔离未知 Event/Field | Event Type 路径 + Event SHA-256 |
| `missing_usage` | Baseline 要求 Usage 但终态缺失 | 固定结构路径 |
| `chunk_limit_exceeded` | Stream 超出探针事件预算 | 固定结构路径 |

Findings 在 Result 中确定性排序、去重，最多 256 个。未知内容只计算 SHA-256，不保存原文；Hash 用于判断同一漂移是否持续出现，不用于恢复内容。

## 6. Outcome 语义

| Outcome | 含义 | 是否协议漂移 |
|---|---|---:|
| `stable` | 请求和结构符合 Baseline | 否 |
| `drift` | 至少一个结构 Finding | 是 |
| `provider_failure` | Adapter 返回安全 NormalizedError | 否，单独做可用性信号 |
| `transport_failure` | 无安全 Provider Error 的网络/读取失败 | 否，单独做网络信号 |
| `timeout` | Probe 自身 Deadline 到期 | 否 |
| `cancelled` | 调度器/进程取消 | 否 |

`provider_failure` 只保留 Code、Category、Retryable 和 HTTP Status，不保留 Safe Message、Provider Message、Request ID 或 Retry Header。这样 429/503 不会被误判为字段漂移，同时仍可供可用性监控聚合。

## 7. Stream 处理

Stream Probe 逐个调用公开 `ChunkStream.Next`，但不累积 Content/Reasoning/Tool Arguments：

- 只检查 Chunk Kind、Finish、Usage 和 Extension；
- `message_end` 缺失会生成结构违规；
- Require Usage 且终态没有 Usage 会生成 `missing_usage`；
- 达到 MaximumChunks 后立即关闭 Stream 并报告漂移；
- Adapter 返回的 ProtocolViolation 直接转为结构 Finding；
- Timeout/Caller Cancellation 保持独立 Outcome。

Body 由 Adapter/ChunkStream 拥有，Runner 仍在调用边界兜底关闭，确保定时任务不耗尽连接池。

## 8. Result 安全模型

`Result` 只有：

- Probe/Provider/Deployment/Adapter/Protocol Version 标识；
- Outcome、Started/Finished/Duration；
- 有界 Findings；
- 可选的安全 ProviderFailure。

类型不存在 Prompt、Response、Raw Body、Provider Message、Tool Arguments、Credential 或 `error` 字段。普通 JSON 序列化因此只产生安全结构；`slog.LogValuer` 进一步只输出身份、Outcome、Duration、Finding Count 和安全错误分类。

Runner 对无效 Probe、Factory/Request 装配失败统一返回 `ErrConfiguration`，不拼接底层 Cause，防止 Secret Resolver 或 Provider SDK Error 泄漏。

## 9. Mock 验证

自动化测试通过 Registry + Mock Factory + 共享 HTTP Handler + 真实 `httptest.Server` 验证：

- 普通与 SSE 稳定 Probe；
- 普通未知顶层字段转为 `protocol_violation`；
- 新 Usage Meter 与新 Finish Reason 产生两个独立 Finding；
- Unknown Stream Field 隔离为带 SHA-256 的 `provider_extension`；
- Stream 缺失 Usage；
- Stream Chunk 上限；
- 429 保持 `provider_failure/rate_limit`；
- Delay 超时与预取消 Context；
- JSON/结构化日志不出现合成 Prompt、Provider 原始 Message 或漂移原值；
- Probe、Baseline、Result 与 Runner Options 非法矩阵。

核心包执行 20 轮重复测试；Linux CI 继续通过 race detector 验证 Runner、Registry、Mock Adapter 和 HTTP 取消路径。

## 10. 生产接入约束

后续周期调度必须满足：

1. 每个 Deployment 设置低频率和随机 Jitter，避免同一时刻放大供应商负载；
2. 使用专用低额度 Project/Budget 与最小权限 Provider Credential；
3. 连续多个相同 Fingerprint 才升级告警，单次网络失败不发布协议版本；
4. Drift 先阻止新协议版本发布，不自动切断所有生产流量；
5. 原始响应只在受控临时诊断中按安全流程获取，不进入 Canary Result；
6. 真实 Provider Key 仅通过 Secret Resolver 注入，不进入仓库、Fixture、命令或 CI 日志。
