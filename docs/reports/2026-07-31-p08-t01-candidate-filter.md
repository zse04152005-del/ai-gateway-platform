# P08-T01 可解释候选过滤器验收报告

- 日期：2026-07-31
- 范围：候选授权、能力、区域、状态、健康、预算与容量过滤
- 结论：工作副本实现和专项验证完成，等待正式仓库门禁与 GitHub Actions

## 1. 策略契约

`CandidateFilter` 对稳定排序后的每个候选严格执行以下顺序，并且只记录第一个拒绝原因：

1. 租户可信访问范围与 Virtual Key 模型白名单；
2. Logical Model 静态能力契约与本次规范化请求的动态能力需求；
3. Logical Model 允许区域；
4. Logical Model、Binding、Deployment、Provider 四层状态；
5. 当前健康资格；
6. 当前预算资格；
7. 当前容量资格。

该顺序是版本化策略 `candidate-filter/v1` 的一部分。前序规则拒绝后不会调用后续依赖，既避免无效调用，也防止后序瞬时状态改变首要解释。

## 2. 可解释性与数据最小化

每个候选产生一个有限枚举结果：

- `tenant_not_allowed`；
- `capability_missing`；
- `region_not_allowed`；
- `inactive`；
- `unhealthy`；
- `budget_denied`；
- `capacity_unavailable`；
- `eligible`。

公开的 `FilterResult` 只序列化 Policy Version 和 `CandidateDecision`。Decision 只包含 Deployment ID、是否合格和原因枚举，不包含 Endpoint URL、Secret Reference、Physical Model、Provider 错误、Prompt 或响应内容。调用方可用 `DecisionFor` 查询单个 Deployment 的结果，也可用 `Clone` 获得深拷贝后交给异步观察链路。P08-T08 再负责把 Route Decision 持久化，本任务不提前引入存储耦合。

## 3. Fail-closed 边界

过滤器先验证四种目录记录的字段与关系，但不会用“必须 active 且已经满足所有策略”的聚合校验掩盖具体拒绝原因。以下情况被视为 CandidateSource 不可信，而不是普通过滤：

- 跨租户或 Logical Model 名称不匹配；
- Binding/Deployment/Provider 关系断裂；
- 字段不合法；
- 同一 Deployment 重复返回；
- 候选超过 256 条安全上限。

健康、预算、容量 Reader 返回错误时分别包装为 `ErrHealthUnavailable`、`ErrBudgetUnavailable`、`ErrCapacityUnavailable`。三者均 fail closed，不把“依赖不可判断”伪装成普通拒绝；Gateway 的既有统一边界会将其映射为不泄漏内部原因的 `ROUTING_UNAVAILABLE`。

Budget/Capacity Reader 只收到 Tenant ID、Project ID、Logical Model、Deployment ID、Provider ID、Stream 标记和 Max Output Tokens。每个 Reader 获得独立的指针拷贝，不能通过修改输入污染另一个 Reader 或原始请求。

## 4. 兼容性与选择行为

`NewSelector(source, health)` 保持 P06 调用方兼容，并显式安装无副作用的 bootstrap Budget/Capacity Reader；后续阶段可用 `NewSelectorWithEligibility` 注入真实实现。Selector 使用过滤结果中的第一个 Eligible 候选，排序仍为 Binding Priority、Provider Code、Deployment Code、Deployment ID，因此既保留原有确定性，又能生成完整候选解释。

## 5. 专项验证

新增和更新的测试覆盖：

- 七种拒绝原因与 `eligible`；
- 严格调用顺序和每一级短路；
- nil 模型白名单继承、空白名单全部拒绝、显式白名单匹配；
- nil Region 不限制、显式 Region 匹配与不匹配；
- 四层目录状态分别禁用；
- Health/Budget/Capacity 的 false 与依赖错误分离；
- 跨租户、关系断裂、重复候选和 257 条越界 fail closed；
- 恰好 256 条候选可处理且报告稳定排序；
- JSON 报告不含敏感字段、结果深拷贝不别名；
- Eligibility 投影最小化和 Reader 间隔离；
- Selector 只从 Eligible 集合按固定优先级选择。

专项包测试连续 10 轮通过，语句覆盖率为 94.8%，`golangci-lint` 专项检查为 0 issue。最终双仓 SHA-256、完整门禁、真实 PostgreSQL 回归、提交、推送与 GitHub Actions 证据只在远端三个 Job 全绿后写入开发执行清单。
