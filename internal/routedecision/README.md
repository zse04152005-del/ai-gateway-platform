# Route decision records

本包保存 P08-T08 的无内容路由解释事实。每次 Selector 评估都按 `request_id + decision_no` 追加记录，并指向它准备创建的 `next_attempt_no`。

持久化内容仅包括候选 Deployment ID、有限过滤原因、过滤策略版本、优先级/权重/随机落点等选择评分、最终 Deployment，以及 `retry.Decision`。每个 Attempt 的分类结果（包括终止重试的 `no_retry`）独立保存；触发重选时同一个安全值也随下一条 Selection 保存，形成完整因果链。不保存 Prompt、响应、Endpoint、Provider Body、Secret Reference、物理模型或私有错误 cause。

`ListByRequestID` 强制 Tenant/Project 双重作用域并按决策序号返回深拷贝，因此后续受权诊断 API 可以复盘初选、排除已尝试 Deployment、无候选和固定策略回退；P16 再负责公开查询接口。
