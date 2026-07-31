# P08-T02 固定、优先级与加权路由验收报告

- 日期：2026-07-31
- 范围：固定 Deployment、稳定优先级、精确权重选择、策略解析、可复现随机源
- 结论：工作副本实现和专项验证完成，等待正式仓库门禁与 GitHub Actions

## 1. 三种策略语义

`RoutePolicy` 是带 Version 的不可变事实，当前只允许三个有限模式：

1. `fixed`：只选择策略指定且已经通过全部过滤的 Deployment。目标不存在或已被健康、预算、容量等规则过滤时返回 `ErrNoCandidate`，不会为了提高表面成功率暗中换到别的模型。
2. `priority`：选择 Eligible 集合中最小 Binding Priority 的候选；同 Priority 按 Provider Code、Deployment Code、Deployment ID 稳定打破平局，保持 P06 行为兼容。
3. `weighted`：在全部 Eligible 候选中按正整数 Binding Weight 做半开区间轮盘选择。候选先稳定排序，再计算累计区间，因此同一候选集、策略和随机落点必然得到同一结果。

固定模式的“不可替换”避免合规或模型质量约束被隐式绕过；加权模式覆盖容量分散、灰度发布和 A/B 测试；优先级模式保留主备路由基础，P08-T07 再把首包前故障切换编排为多个独立 Attempt。

## 2. 策略来源与 fail-closed

`PolicyResolver` 只接收 Tenant ID、Project ID、Logical Model 三个无内容字段。Selector 先完成 P08-T01 全候选过滤；没有 Eligible 候选时直接返回，不调用策略或随机依赖。存在候选时才解析策略并重新校验：

- Version 必须为 1～128 位规范标识；
- 固定模式必须包含规范的小写 Deployment UUID；
- 优先级/加权模式禁止携带 Fixed Deployment，消除歧义；
- 未知模式、Resolver 错误或畸形策略统一 fail closed 为 `ErrPolicyUnavailable`。

Weighted RandomSource 返回错误或越过请求的 `[0,totalWeight)` 边界时，统一 fail closed 为 `ErrRandomUnavailable`。单 Eligible 候选不消费随机数，避免无意义的依赖故障。进程默认仍由 `bootstrap-priority/v1` 提供优先级策略；正式多租户策略发布与持久化不塞入环境变量，由后续配置发布阶段接管。

## 3. 可注入随机种子

`NewSeededRandom(seed)` 使用由一个显式 `uint64` Seed 派生的 PCG Stream。内部 Mutex 保护状态，因此同一个实例可以被并发 Selector 安全共享；相同 Seed 产生完全相同的序列，不同 Seed 产生不同序列。该随机数只用于流量分配，不用于 Key、Nonce、Token 或其他安全决策。

生产默认随机源和 Seeded Random 都返回无偏的 `Uint64N(totalWeight)`，没有 `% totalWeight` 的取模偏差。测试还用可注入精确落点覆盖每个权重区间边界，避免仅靠概率测试掩盖 off-by-one。

## 4. 可解释选择结果

`Selection.Decision` 返回可安全记录的：

- Policy Version 与 Mode；
- Selected Deployment ID；
- 选中候选的 Priority 与 Weight；
- Eligible Candidate Count；
- Weighted 模式的 Total Weight 与 Random Draw。

Decision 可深拷贝且 JSON 中没有 Endpoint、Secret Reference、Physical Model、Provider ID、Prompt 或响应内容。P08-T08 将把它与 P08-T01 的候选过滤报告合并后按 Request ID 持久化；本任务只建立无存储耦合的可复盘事实。

## 5. 专项验证

新增和更新的测试覆盖：

- 固定目标忽略 Priority，但目标被过滤或不存在时绝不替换；
- 默认与显式优先级模式保持稳定排序和完整 Decision；
- 1:3:6 权重的 `[0,1)`、`[1,4)`、`[4,10)` 五个边界落点；
- 20,000 次固定 Seed 采样符合 1:3:6 分布；
- 相同/不同 Seed 序列、零上界、nil 随机源和 32 路并发共 32,000 次读取；
- Policy Version、Mode、Fixed ID 与歧义字段验证；
- Resolver 错误、畸形策略、Random 错误与越界落点 fail closed；
- 无 Eligible 候选时策略/随机依赖零调用；
- 单 Weighted 候选不消费随机数；
- Policy Query 与 Decision 数据最小化、Decision 深拷贝和构造器 nil 边界。

Routing 专项测试连续 10 轮通过，语句覆盖率为 95.6%，`golangci-lint` 专项检查为 0 issue。最终双仓 SHA-256、完整门禁、真实 PostgreSQL 回归、提交、推送与 GitHub Actions 证据只在远端三个 Job 全绿后写入开发执行清单。
