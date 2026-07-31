# 首次客户端输出前的故障切换编排

> 状态：Implemented
> 对应任务：P08-T07
> 编排策略：`retry-classifier/v1` + `candidate-filter/v1`

## 1. 目标和适用边界

一个客户端请求可以产生多个物理 Provider Attempt，但每次 Attempt 都必须有独立身份、Deployment、状态、Usage 与结束原因。自动切换只发生在客户端尚未收到模型输出时；任何已提交的内容、推理或工具 Delta 都使响应不可逆。

当前公开非流式 Chat 路径采用完整响应缓冲，因此 Gateway 在成功响应投影和终态记录完成前不会写出第一个客户端字节，所有自动切换天然处在首次客户端输出之前。流式语义继续由 P07 的 `streaming.FailoverGate` 保证：旧 Attempt 的首模型 Delta 与备用 Attempt 原子竞争，首输出后不能拼接；公开 `stream=true` Handler 仍保持显式 501，不能把尚未接线的流式传输描述成生产可用。

## 2. 请求级硬边界

默认生产策略：

| 约束 | 默认值 | 硬边界 |
|---|---:|---:|
| 最大物理 Attempt | 3 | 1～32 |
| 覆盖路由和所有 Attempt 的总时限 | 30 秒 | 正数且不超过 24 小时 |
| 下一 Attempt 最小有效窗口 | 250 毫秒 | 正数、小于总时限且不超过 10 分钟 |
| 额外费用许可 | 显式 allowed | 未提供或 denied 不重试 |

编排是一个单循环，没有递归重试、Adapter 内重试或每层独立 Attempt 预算。第 N 个 Attempt 只能在第 N-1 个 Attempt 已持久化终态后开始，因此故障风暴中的调用数严格不超过 `MaximumAttempts`。

## 3. 顺序执行流程

1. GatewayRequest 完成 `authorized → routing`，再创建覆盖路由与全部 Attempt 的总 Deadline。
2. Selector 生成初始 Selection；选路失败在任何 Provider I/O 前终止 Request。
3. `StartAttempt` 原子增加 `attempt_count` 并生成新的 RouteAttempt UUID，再允许 Provider I/O。
4. 单 Attempt Executor 完成真实调用；非流式结果先完成公共协议投影，但尚不写客户端。
5. 成功时 Attempt 与 Request 原子终结为 succeeded，响应中的 `gateway.attempt_count` 使用真实物理尝试数。
6. 失败时调用 P08-T06 分类器。`no_retry` 直接终结；其余动作先调用 `CompleteAttemptForRetry`，把当前 Attempt 终结为 retryable_failed，而父 Request 保持 running。
7. 等待经过预算验证的 `Retry-After`，再进行下一次 Selection。
8. 新 Selection 成功后回到步骤 3；没有备用目标、总预算耗尽、调用方取消或记录失败时终止 Request。

任何数据库记录失败都 fail closed。特别是 `StartAttempt` 失败时不调用 Provider；Attempt 已调用但终态记录失败时返回统一记录依赖错误并保留可恢复的 active 事实，不能继续启动一个无法审计的新 Attempt。

## 4. Deployment 排除与策略语义

`SelectionRequest.ExcludedDeploymentIDs` 是最多 32 个、合法且唯一的 Deployment UUID。候选过滤器在目录状态校验后、健康/预算/容量读取前，以有限原因 `previously_attempted` 排除目标，既避免重复 I/O，也保留 P08-T08 可复盘证据。

- `different_deployment_only`：必须排除全部已经尝试的 Deployment；Selector 若返回旧目标，编排器按内部不变量错误 fail closed。
- `retry_allowed`：先排除已尝试目标，优先使用其他健康 Deployment；若没有替代目标，再允许按原发布策略重选，包括固定策略中的同一目标。复用目标不会再次加入排除集合，因此排除输入始终合法、唯一，重试仍由 Attempt 上限终止。
- Fixed Policy：不同 Deployment 限制不会暗中突破固定目标；若故障只允许换目标，则停止。
- Priority/Weighted Policy：在排除后的完整 Eligible 集合上重新执行原策略，Weighted 会产生新的安全随机决策事实。

每次重选都重新执行当前健康、熔断、预算与容量检查。Circuit Permit 仍只在最终选中且真正调用 Provider 前原子获取；扫描候选不会占用 Half-Open 名额。

## 5. Request/Attempt 事务语义

`CompleteAttemptForRetry` 在一个 PostgreSQL 事务中完成可选 `connecting → headers_received` 和终态 `retryable_failed`，但不修改父 Request。下一次 `StartAttempt` 使用仍为 running 的父 Request CAS 版本，执行 `running → running`、`attempt_count + 1` 并创建新 UUID。最终 `CompleteAttempt` 才同时终结 Request。

数据库事件链因此可以是：

```text
Request: authorized → routing → running → running → succeeded
Attempt 1: created → connecting → [headers_received] → retryable_failed
Attempt 2: created → connecting → headers_received → succeeded
```

旧 Attempt 一旦终结就不能覆盖，新 Attempt 也不能复用 `(request_id, attempt_no)`。Recorder 的乐观版本与数据库 Trigger 同时阻止并发旧协程改写终态。

## 6. Usage 与费用完整性

成功、失败、取消和部分失败 Attempt 都允许保存各自已知的 presence-preserving Usage Summary。若 Provider 调用成功但 Gateway 在客户端投影前发现协议问题，已知 Provider Usage 和 Provider Request ID 会附在该失败 Attempt 上，再考虑备用目标；不会因为响应未发送给客户端就假设该 Attempt 免费。

公共 OpenAI `usage` 仍只描述最终成功模型响应，不把不同模型/Provider 的 Token 机械相加；真实 `gateway.attempt_count` 提示发生过多次物理调用。P10-T06 将按全部 RouteAttempt 的独立 Usage/价格版本聚合费用。P08-T07 的保证是任何物理调用都有独立可计费事实，后续聚合不能因为透明切换而遗漏 Attempt。

## 7. 失败和取消

- 无备用 Deployment：父 Request 以 `failover_exhausted` 终止，对外保留最后一个真实 Provider 错误，不用内部“无候选”覆盖根因。
- 重选基础设施失败：父 Request 以 `failover_routing_failed` 终止，对外返回安全 `ROUTING_UNAVAILABLE`。
- 客户端取消/Deadline：当前 Provider Context 立即取消；不会进入下一 Attempt。
- 总重试 Deadline：停止等待或执行，Request 以有限原因终止。
- `Retry-After`：已经由分类器确认能容纳等待和最小执行窗口，实际等待仍响应 Context 取消。

所有公开错误继续使用固定 Envelope；原始 error、Provider Body、Endpoint、Secret、Prompt 和失败字符串不会进入响应或安全重试决策。

## 8. 验证重点

- Transport 后 A→B 切换，两个独立 Attempt 且响应 count=2；
- 认证/权限/参数/内容拒绝只调用一次；
- `different_deployment_only` 无备用时绝不回到 A；
- 429 先选择不同目标，无替代时才允许固定目标重试；
- 三次故障严格产生 3 个 Provider 调用、2 个中间终态和 1 个最终终态；
- 已知失败 Usage 深拷贝并持久化，原始 Raw Evidence 不进入 Summary；
- 真实 PostgreSQL 验证父 Request 保持 running、Attempt 编号单调、两个 Usage Summary 均存在和完整事件链。
