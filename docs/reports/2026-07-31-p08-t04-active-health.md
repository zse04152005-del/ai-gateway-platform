# P08-T04 主动健康检查验收报告

- 日期：2026-07-31
- 范围：探针目录、低成本请求、独立连接池、抖动调度、被动样本门控、迟滞状态与 Gateway 生命周期
- 结论：工作副本实现和专项验证完成，等待正式仓库门禁与 GitHub Actions

## 1. 市场痛点与实现取舍

主动健康检查常见但容易制造新的生产问题：对每个模型固定频率轮询会消耗付费 Token 和供应商配额；多副本同时重启会形成探针风暴；监控链路故障若被当成 Provider 故障，可能一次性摘除全部路由。本实现把主动健康定位为被动统计的“冷路由补盲”，而不是重复制造热路由流量。

目录只发布 Provider/Deployment active、具备 Chat 能力，并且存在 active Tenant/Project/Logical Model/Binding 授权链的物理 Deployment。多租户共享 Deployment 只探测一次，未发布或不可路由的 Deployment 不产生费用。目录快照先完整验证再原子替换，错误快照不会清空最后一次可信目标集。

## 2. 低成本与反风暴调度

默认每个 Deployment 5 分钟一次，启动相位在完整 5 分钟窗口内按 Deployment ID 稳定散列，后续周期带 ±20% 的确定性抖动。目录每 30 秒刷新，调度分辨率 1 秒；最多 4 个 Worker、每批 16 个目标、单次 5 秒，目标集硬上限 10,000。

在真正发请求前，Scheduler 查询 `PassiveHealth.NeedsActiveProbe`：滑动窗口只要存在成功、429、5xx 或 Provider Timeout 样本，就说明真实流量已经覆盖健康测量，主动探针被抑制；样本过期后自动恢复探测。Caller Cancellation 与本地配置/协议错误不算 Provider 健康样本，因此不会错误关闭冷路由补盲。门控读取失败只增加有限计数并跳过本次付费请求，不制造负面健康事实。

## 3. 探针与生产隔离

探针使用真实 Adapter/Secret Reference 解析路径，但请求内容固定为 `ping`、`temperature=0`、`max_output_tokens=1`，不携带 Tenant、Project、Virtual Key、Tools、媒体、Provider Options 或用户数据，并带 `X-AI-Gateway-Traffic-Class: active-health/v1`。

Gateway 为探针创建独立 HTTP Transport/连接池，每 Host 最多 2 个连接；探针不进入公开 Handler、认证、业务 Selection、重试、GatewayRequest/RouteAttempt、计费事件或被动观察器。由此隔离应用记账和连接池资源。若企业要求 Provider 账户层的额度/账单也完全隔离，仍须后续增加专用 Probe Credential Reference 或接入供应商原生无计费健康端点；当前实现明确记录该边界，不把同一 Provider 账户虚假描述为独立配额。

## 4. 状态迟滞与故障包含

Active Tracker 使用有限状态 `unknown/healthy/unhealthy/stale`：

- 未探测目标默认 eligible，避免启动时全量黑洞；
- 连续 3 次失败才标记 unhealthy；
- unhealthy 后连续 2 次成功才恢复；
- 超过 20 分钟无新证据进入 stale，并 fail open；
- Map 有 10,000 目标硬上限和确定性最旧状态淘汰；
- 计数采用饱和递增，避免回绕。

`routing.CompositeHealth` 对 Passive 与 Active 做 AND 组合；任一可信当前信号不健康都会过滤 Deployment，读取错误继续 fail closed。Active stale 单独 fail open，避免数据库、调度器或探针凭据故障误杀所有 Provider。进程关闭会取消在途探针并等待 Worker；关闭导致的 Cancellation 不记为 Provider 失败。

## 5. 专项验证

测试覆盖：

- 失败阈值、恢复阈值、Unknown/Stale fail-open 和确定性淘汰；
- 固定 `ping`/1 Token 请求、Traffic Class Header、真实 Mock Adapter/HTTP 成功路径；
- Provider、Transport、Protocol、Deadline 与 Cancellation 有限分类；
- 全窗口启动打散、±20% 稳定抖动、批量上限与并发上限；
- 热路由探针抑制、门控错误跳过、重复 Scheduler 防护与取消收敛；
- 目录 Target 生命周期、关系、能力与深拷贝；
- PostgreSQL 只返回实际可路由目标、跨租户共享去重、Tenant 暂停后自动收敛；
- Passive + Active 组合顺序、短路与依赖错误 fail-closed；
- Gateway 装配独立连接池并与进程生命周期一起关闭。

Activehealth 语句覆盖率为 82.8%，Routing 为 96.1%，专项 `golangci-lint`（含 integration build tag）为 0 issue。最终双仓 SHA-256、完整门禁、真实 PostgreSQL 回归、提交、推送与 GitHub Actions 证据只在远端三个 Job 全绿后写入开发执行清单。
