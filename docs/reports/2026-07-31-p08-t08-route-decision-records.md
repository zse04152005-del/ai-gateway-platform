# P08-T08 路由决策解释记录验收报告

- 日期：2026-07-31
- 范围：候选过滤、策略评分、最终选择、重试因果链、Tenant/Project 作用域复盘
- 结论：实现与本地完整门禁通过，GitHub Actions 证据在远端全绿后写入开发执行清单

## 1. 实现结果

新增 `internal/routedecision` Store 与迁移 `000009_create_route_decisions`。每次 Selector 评估在 Provider I/O 前写入 `route_decisions`，每个失败 Attempt 的重试分类写入 `route_retry_decisions`；最终 `no_retry` 不再只存在于进程内。

一次复盘可以看到候选 Deployment ID、有限过滤原因、过滤策略版本、路由模式、优先级、权重、eligible count、weighted random draw、最终选中 Deployment，以及触发后续重选的前序 RetryDecision。查询通过父 GatewayRequest 强制 Tenant/Project 双重作用域并返回深拷贝。

## 2. 安全与一致性

记录模型和数据库约束共同阻止未知枚举、非法 UUID、重复候选、eligible/reason 冲突、非法 weighted draw、策略版本不一致、过大 JSON 和错误 Attempt 序号。空候选明确编码为 `[]`，不会以 JSON `null` 绕过数组契约。

RouteDecision 写入失败时不会创建 Attempt 或调用 Provider；RetryDecision 写入失败时会终结当前 Attempt 并禁止下一 Attempt。记录内容不包含 Prompt、响应、Endpoint、Provider Body、Secret Reference、物理模型或私有 error cause。

## 3. 自动化覆盖

单元测试覆盖：

- `FilterResult.ValidateExplanation` 的有效、非法 UUID、未知原因、eligible 冲突与重复候选；
- `PolicyDecision.Validate` 的 fixed/priority/weighted 正反例和单候选无 draw 边界；
- `retry.Decision.Validate` 的有限枚举、Attempt 边界、负预算与首输出不可逆边界；
- 初选与重选决策因果链、终止型 `no_retry`、RouteDecision/RetryDecision 存储失败的 fail-closed 行为；
- Selection 与 Record 深拷贝。

真实 PostgreSQL 集成覆盖：

- `decision_no` 与 `next_attempt_no` 单调序列；
- A 被标记 `previously_attempted` 后选择 B；
- RetryDecision 独立读取及跨 Tenant/Project 查询隔离；
- 空候选 `[]` 与 JSON Schema 约束；
- 非流式 E2E 的 RouteDecision/RetryDecision 数量和最终 `no_retry`。

## 4. 发布边界

本任务提供可由受信组件按 `requestId` 复盘的持久化 Reader，不新增公开管理端点。P16-T06 负责公开查询 API、管理面授权、分页和响应投影，不能把底层 Store 的存在等同于外部 API 已上线。

## 5. 本地门禁结果

- Routing/Retry/RouteDecision/Gateway 专项连续 20 轮通过；
- 覆盖率：94.5% / 98.2% / 13.5% / 81.1%，其中 PostgreSQL Store 主体由 integration build tag 套件覆盖；
- 常规与 integration lint 均为 0 issue；
- 完整 `scripts/dev.ps1 -Action check` 通过，包括单测、race、漏洞扫描、迁移连续性和本地高风险 Secret 扫描；
- `migration validation passed: count=9 latest=000009_create_route_decisions`；
- `scripts/dev.ps1 -Action test-integration` 在真实 PostgreSQL 上全量通过。
