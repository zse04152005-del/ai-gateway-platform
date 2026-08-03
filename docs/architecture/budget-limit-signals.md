# 软预算告警与硬预算错误

P09-T10 为预算 admission 提供统一、可安全序列化的额度信号。soft crossing 不拒绝请求，但返回结构化 `LimitNotice`；hard crossing 返回仍可通过 `errors.Is(err, ErrBudgetExceeded)` 判断、并可通过 `errors.As(err, *HardLimitError)` 读取相同字段的安全错误。

## 1. 统一载荷

soft 与 hard 共用以下客户端元数据：

```json
{
  "level": "soft|hard",
  "remaining_micros": 30,
  "reset_at": "2026-08-03T16:00:00Z",
  "degradation_hint": "wait_for_reset"
}
```

`remaining_micros` 是当前 Account hard 以内仍可 admission 的精确整数额度，最小为 0；`reset_at` 是该 Account 半开周期的 `period_end`，不使用临时 Worker 时钟推算。soft Notice 使用成功 reserve Ledger 的 resulting committed+reserved 计算剩余，保证幂等重放返回原 admission 时点的额度；hard Error 使用拒绝前最新 Account 快照计算剩余，不写入任何新事实。

恰好等于 soft 不告警，只有 resulting spend 严格大于 soft 才设置 `SoftLimitExceeded=true` 并附带 `LimitNotice{level=soft}`。超过 hard、单次请求本身大于 hard，或 actual overage 已使 committed 达到/超过 hard 时，`HardLimitError.Notice.RemainingMicros` 分别返回真实剩余或 0。

## 2. 有限降级提示

降级提示由可信策略作为 `ReserveInput.DegradationHint` 传入，只接受三个有限动作码：

| Code | 含义 |
| --- | --- |
| `use_lower_cost_model` | 使用策略已授权的低成本逻辑模型 |
| `reduce_max_output` | 减少最大输出 allowance 后重新 admission |
| `wait_for_reset` | 等待返回的 reset boundary |

空值合法，表示当前没有安全降级建议。调用方负责把动作码本地化为用户提示，不能把模型名、Tenant、Account、Project、Key、Request 或任意自由文本塞入该字段。非法提示在数据库访问前以 `ErrInvalid` 拒绝。

## 3. 错误与隔离边界

`HardLimitError.Error()` 保持稳定泛化字符串 `budget hard limit exceeded`，底层余额、DSN 和 SQL 不进入错误文本；结构化 Notice 只包含 level、remaining、reset 和有限 hint。它不包含任何资源身份，因此可直接映射到 API error details。序列化前仍应调用 `Notice.Validate()`，防止手工构造的非法值进入响应。

只有在 Tenant-qualified Account 成功读取后才会创建 `HardLimitError`。伪造其他 Tenant 并携带真实 Account ID 时仍返回 `ErrAccountNotFound`，不会返回 remaining、reset 或 hint，从而不能用额度差异探测其他租户账户。hard 拒绝不推进 version，也不创建 Reservation/Ledger。

## 4. 调用约定

调用方处理顺序：

1. `Reserve` 成功后检查 `result.LimitNotice`；soft Notice 可记录告警或向同一可信 Principal 展示，但请求继续执行；
2. 失败先用 `errors.Is` 分类；只有 `ErrBudgetExceeded` 再用 `errors.As` 取得 `HardLimitError`；
3. 根据有限 hint 决定是否尝试经授权的降级，任何降级都必须重新执行完整 admission，不能复用被拒绝的结果；
4. 对 `ErrAccountNotFound`、inactive、conflict 或 unavailable 不返回额度细节。

## 5. 验收边界

真实 PostgreSQL 测试在 soft=60、hard=100 的 Account 上依次 reserve 60、10 和 31 micros：第一笔恰好 soft 无 Notice，第二笔成功并返回 soft/remaining=30/reset/hint，第三笔 hard 拒绝并返回同样 remaining/reset 及选定 hint。伪造另一个 Tenant 查询同一 Account 只能得到 not found；错误 JSON 和 Error string 均不得包含 Tenant/Account/Request 或作用域字段，数据库最终仍只有两条 Reservation/Ledger。
