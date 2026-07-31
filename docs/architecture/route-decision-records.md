# 路由决策解释记录

> 状态：Implemented
> 对应任务：P08-T08
> 数据版本：`000009_create_route_decisions`

## 1. 目标与边界

P08-T08 保存每次 Selector 评估时已经形成的安全事实，使受权调用方能够按 `requestId` 复盘初选、故障后的排除与重选、固定目标复用以及最终停止重试的原因。记录服务不重新执行历史策略，也不读取当前目录来猜测过去的结果。

本阶段交付的是内部持久化与受信 Reader 接口。公开的 `/admin/v1/route-decisions/{requestId}` 诊断 API、授权模型和响应契约仍由 P16-T06 实现。

## 2. 事实模型

`app.route_decisions` 以 `(request_id, decision_no)` 为主键，按请求保存每次选路评估：

- `next_attempt_no`：本次选择准备创建的物理 Attempt 序号；
- `outcome`：`selected`、`no_candidate` 或 `selection_failed`；
- `filter_policy_version` 与 `candidate_decisions`：每个候选 Deployment ID、是否可用和有限的首个过滤原因；
- `route_policy_version` 与 `policy_decision`：模式、优先级、权重、候选数、加权总和与随机落点；
- `retry_policy_version` 与 `retry_decision`：触发本次重选的前序安全重试结论；
- `selected_deployment_id` 与 `decided_at`：最终选中目标和决策时间。

`app.route_retry_decisions` 以 `(request_id, attempt_no)` 为主键，保存每个失败 Attempt 的分类结论。终止型 `no_retry` 也必须记录，因此“最后为何停止”和“中间为何继续”都可单独复盘。成功 Attempt 不需要虚构 retry 分类。

两类 JSON 均有类型和大小约束，Go 层还验证有限枚举、规范 UUID、候选唯一性、eligible/reason 一致性、策略模式与评分字段一致性，以及 Attempt 序号匹配。

## 3. 写入顺序与故障语义

正常调用顺序为：

1. Selector 计算候选过滤和策略选择；
2. RouteDecision 在 Provider I/O 前追加；
3. RouteAttempt 创建并开始物理调用；
4. 失败后 RetryDecision 在 Attempt 结束前追加；
5. 当前 Attempt 终结；仅当结论允许时才执行下一次 Selector。

RouteDecision 写入失败时 fail closed，不创建对应 Attempt，也不调用 Provider。RetryDecision 写入失败时，当前 Attempt 仍按原始故障终结，但禁止创建下一 Attempt，并向客户端返回统一的记录服务不可用错误。这样不会出现已付费调用没有 Attempt 身份，也不会在缺少重试审计事实时继续放大请求。

Selection 记录与 Attempt 创建有意分离。进程若在二者之间崩溃，记录仍能准确表达“已选中，但尚未开始物理调用”，而不是伪造一个不存在的 Attempt。

## 4. 故障切换复盘

一次 A→B 切换形成如下因果链：

1. `decision_no=1, next_attempt_no=1`：A 与 B 均 eligible，策略选择 A；
2. `attempt_no=1`：分类为 `different_deployment_only`；
3. `decision_no=2, next_attempt_no=2`：A 为 `previously_attempted`，策略选择 B，并附带步骤 2 的安全 Decision；
4. `attempt_no=2`：若最终失败，则保存终止型 `no_retry`；若成功，则由 Attempt 成功事实闭环。

普通 `retry_allowed` 在固定策略下可能先形成一条 `no_candidate`，随后按显式策略移除排除并记录同目标复用。每次 Selector 调用都有独立 `decision_no`，因此不会把中间失败的评估吞掉。

## 5. 隔离与隐私

Reader 查询必须同时提供可信 `TenantID + ProjectID`；Store 通过父 `gateway_requests` 校验双重作用域，跨域请求与不存在请求统一返回 `ErrNotFound`。返回值深拷贝，调用方不能修改 Store 内部事实。

持久化边界禁止 Prompt、响应内容、Endpoint、Provider Body、Secret Reference、物理模型名和私有错误 cause。过滤原因、路由模式、重试动作与失败类别全部使用有限枚举，不允许把任意错误字符串伪装成诊断字段。

## 6. 生命周期

记录通过父 Request 和 Attempt 外键保持引用完整性，属于追加式执行证据。迁移 down 先删除 RetryDecision，再删除 RouteDecision；生产环境仍遵循前滚修复，不把有数据丢失风险的 down 当作常规操作。

P13 的可观测性与 Reconciler 可以消费这些内部事实；P16-T06 再在严格管理面授权、审计和分页边界下公开查询能力。
