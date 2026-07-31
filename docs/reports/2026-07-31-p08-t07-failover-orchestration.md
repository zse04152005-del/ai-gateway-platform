# P08-T07 首次输出前故障切换验收报告

- 日期：2026-07-31
- 范围：有界顺序重试、跨 Deployment 重选、独立 Attempt、父 Request 连续性、Usage/费用事实
- 结论：工作副本实现和专项验证完成，等待正式仓库门禁与 GitHub Actions

## 1. 编排结果

Gateway 非流式生产链路现已接入 `retry-classifier/v1`。默认最多 3 个 Attempt、30 秒请求总时限和 250ms 下一 Attempt 最小窗口；路由、Provider I/O、`Retry-After` 等待和所有切换共享同一 Deadline。编排只有一个顺序循环，不存在 Adapter 内部或递归重试。

每次失败先完成当前 Attempt，再重选和启动下一 Attempt。`different_deployment_only` 排除全部已尝试目标；普通 retry 也优先排除旧目标，仅在不存在替代候选时允许按原策略复用。已尝试目标按有序集合维护，同一固定目标被复用多次也不会生成重复排除项或破坏后续重选。候选报告新增 `previously_attempted`，固定路由不会被“自动容灾”暗中突破。

## 2. 持久化与费用事实

Recorder 新增 `CompleteAttemptForRetry`：事务化终结当前 Attempt 为 retryable_failed，同时保持父 Request running。下一次 `StartAttempt` 生成新 UUID 和递增 AttemptNo，最终 Attempt 才与 Request 同时终结。失败 Attempt 现在可以保存已知 Usage；Provider 已返回有效计量但客户端投影失败时，Usage 和 Provider Request ID 仍归属于该失败 Attempt。

公共 `gateway.attempt_count` 改为真实物理调用数。公共 `usage` 仍属于成功模型响应，P10 再按全部独立 Attempt 和价格版本聚合费用；当前任务已经消除透明切换导致物理调用或 Usage 被覆盖的缺口。

## 3. 不可逆边界

非流式响应在完整 Normalized Response 通过公共投影、最终 Attempt 记录成功前不写任何客户端字节，因此所有自动切换均发生在首次客户端输出前。流式侧继续使用 P07 `FailoverGate` 的原子首模型 Delta 边界；公开流式 Handler 尚未接线，仍显式 501，不伪装为已上线。

## 4. 专项验证

自动化覆盖 A→B 切换、确定性拒绝不重试、不同 Deployment 无备用停止、429 优先备用后固定目标复用、同一目标连续失败仍严格受三次上限约束、排除集合不重复、私有错误不进入成功响应、真实 attempt_count、已知失败 Usage 深拷贝、非法配置和取消等待。真实非流式 E2E 同时验证 429/容量错误在固定目标上有界重试后仍保留原始公共 Provider 错误，且每次物理调用均形成独立审计 Attempt。

Gateway/Execution/Routing 专项连续 20 轮通过，覆盖率分别为 81.1%/34.2%/94.3%，专项 lint 0 issue。真实 PostgreSQL 新增双 Attempt 回归，验证 Request `authorized→routing→running→running→succeeded`、两个独立 Attempt 状态链、AttemptNo 1/2、不同 Deployment 和两个非空 Usage Summary。最终双仓 SHA-256、完整门禁、PostgreSQL 全量回归、提交、推送与 GitHub Actions 证据只在远端三个 Job 全绿后写入开发执行清单。
