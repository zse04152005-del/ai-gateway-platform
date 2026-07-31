# 重试分类与请求级预算

> 状态：Implemented
> 对应任务：P08-T06
> 策略版本：`retry-classifier/v1`

## 1. 目标与边界

分类器只回答“上一个物理 Attempt 失败后，策略是否允许再尝试”，不执行等待、不选择 Deployment、不创建 Attempt，也不修改熔断、健康或账本。P08-T07 负责消费判定并编排新的物理 Attempt；P08-T08 负责把安全判定与候选/策略事实组合后持久化。

这个边界避免把错误语义、路由选择和 I/O 生命周期耦合成一个难以验证的循环，也保证所有普通与流式调用共享同一套重试上限。

## 2. 显式输入

每次分类必须提供：

| 输入 | 含义 | Fail-closed 规则 |
|---|---|---|
| `Failure` | 上一 Attempt 的受信错误对象 | nil 或未知类型不产生可重试推断 |
| `ModelOutputStarted` | 是否已输出客户端可见的内容/推理/工具 Delta | true 永不重试 |
| `Submission` | `not_submitted` / `submitted` / `unknown` | 未显式提供即拒绝分类 |
| `AttemptNumber` / `MaximumAttempts` | 当前物理 Attempt 与请求级硬上限 | 最大 32；达到上限停止 |
| `Now` / `Deadline` | 统一请求总时限，而非单 Attempt 时限 | 已耗尽停止；未来跨度最大 24 小时 |
| `MinimumAttemptWindow` | 下一次 Attempt 至少需要的有效执行时间 | 必须为 0～10 分钟内的正值 |
| `AdditionalCost` | 额外费用是否已经被上层预算允许 | denied 停止；不得靠零值隐式放行 |

`Retry-After` 只从已验证的 `adapter.NormalizedError` 读取，且不会从 Header 或 Provider Body 二次解析。下一 Attempt 只有在 `Retry-After + MinimumAttemptWindow <= Deadline - Now` 时才允许；相等边界允许，超过 1ns 也拒绝。

## 3. 判定矩阵

| 故障 | 默认动作 | 说明 |
|---|---|---|
| 客户端可见模型输出已经开始 | `no_retry` | 不可逆最高优先级，不拼接模型 |
| Caller Cancellation / Caller Deadline | `no_retry` | 不把调用方终止伪装成 Provider 故障 |
| Auth / Permission | `no_retry` | 换路由不能修复调用身份或供应商凭据问题 |
| Invalid Request / Context Length / Content Policy | `no_retry` | 请求或策略确定性拒绝 |
| Unknown / 本地 Adapter 配置 | `no_retry` | 无可靠瞬时故障证据，安全停止 |
| 429 / Capacity 且 Provider 标为 retryable | `retry_allowed` | 仍受 `Retry-After` 和全部预算门禁 |
| Timeout / 临时 5xx 且 Provider 标为 retryable | 视提交状态 | 未提交可重试；已提交或未知仅换 Deployment |
| Transport | 视提交状态 | 未提交可重试；已提交或未知仅换 Deployment |
| Protocol | `different_deployment_only` | 避免在同一不兼容 Deployment 重复失败 |
| 首 Token 超时且尚无模型输出 | `different_deployment_only` | 只允许透明切换备用 Deployment |
| no-progress / 首输出后的总超时 | `no_retry` | 归为部分失败，不继续拼接 |

“Provider 标为 retryable”是 Adapter 契约中的受验证事实。429、Capacity、Timeout 或 5xx 若明确为不可重试，分类器不会自行推翻 Adapter 判定。协议故障是例外：它只允许换 Deployment，不允许在同一个不兼容目标重复执行。

## 4. 预算门禁顺序

候选可重试错误依次经过：

1. 客户端可见模型输出不可逆边界；
2. 最大 Attempt 数；
3. 额外费用许可；
4. 请求总 Deadline；
5. 下一 Attempt 最小有效窗口；
6. Provider `Retry-After`；
7. 提交不确定性导致的“仅换 Deployment”收窄。

语义上本就不可重试的认证、参数或内容拒绝不会被预算原因覆盖，从而保留准确复盘原因。对可重试候选，任一预算耗尽都把动作收敛为 `no_retry`。

## 5. 重复计费风险

提交状态不是根据错误字符串猜测，而是由执行层显式传入：

- `not_submitted`：可以证明请求未越过上游提交边界；
- `submitted`：已经提交，可能产生费用或副作用；
- `unknown`：连接中断等场景无法证明是否提交，按最保守状态处理。

Timeout、5xx 和 Transport 在 `submitted/unknown` 时只能切换不同 Deployment。该约束不能消除原 Attempt 已计费的可能性，但能阻止对同一异常目标进行局部循环；所有实际 Attempt 仍必须进入后续总费用聚合。

## 6. 安全与可观测性

`Decision` 可 JSON 序列化，但只包含策略版本、动作、原因、有限故障类、提交状态、Attempt 计数、剩余预算毫秒、要求等待毫秒和输出边界布尔值。毫秒统一向上取整，避免展示比真实约束更宽松的预算。

Decision 不保存原始 `error`、`Error()` 字符串、Provider Body、Endpoint、Secret Reference、Prompt、Completion、Provider Request ID 或内部 cause。非法输入只返回稳定的 `retry.ErrInvalid`；调用方必须把它视为不重试。

## 7. 与相邻组件的契约

- Adapter/Proxy：只提供已验证 `NormalizedError` 或稳定的 Transport/Protocol/Adapter 类型。
- Streaming：只信任 `TimeoutFailure` 中的首输出与首 Token 前可重试事实。
- P08-T07：只有 `retry_allowed` 或 `different_deployment_only` 才能创建下一 Attempt；后者必须排除上一 Deployment。
- P08-T08：持久化 Decision 值，不持久化 Failure。
- P09/P10：预算预留与所有 Attempt 费用聚合接入后，负责产生 `AdditionalCost` 许可，不能绕过当前硬门禁。
