# Price Version Model

> 状态：Implemented
>
> 日期：2026-08-03
>
> 对应任务：P10-T03

## 1. 版本身份与生效边界

`app.price_versions` 是一次价格发布，不是可原地覆盖的“当前价格”。每行绑定一个物理 Deployment、该 Deployment 的 Region、三位大写币种代码和 `effective_at`。同一 Deployment/Region/生效时间只能有一个版本；选择器只能从 `published` 中取 `effective_at <= observed_at` 的最新记录。

版本必须以 `draft/version=1` 创建，至少包含一条费率后才能一次性转为 `published/version=2`。发布时 Deployment、Region、Currency、生效时间、创建时间和创建者均不可变化；发布后任何再次更新或删除都会被数据库拒绝。

## 2. Token 类型与单位

`app.price_version_rates` 以 `(price_version_id, token_type)` 为主键，每个版本对一个 Token 类型最多一条费率。`unit_quantity` 是报价单位数量，`unit_price_micros` 是该数量对应的整数 micros 价格，二者均限制在 JSON 可精确交换的整数范围内。

| Token 类型 | 合法计费单位 |
|---|---|
| input/output/cache_read/cache_write/reasoning | `token` |
| audio_input/audio_output | `token` 或 `second` |
| image_input/image_output | `token` 或 `image` |

版本可以只发布 Provider 真正支持的维度；缺少对应 Token 类型费率时，Usage 的复合外键会 fail closed，不能套用其他维度或猜测价格。费率只能在父版本为 draft 时追加，并先锁定父版本行，使并发发布与追加具有确定顺序；费率自身禁止 UPDATE/DELETE。

## 3. 历史请求锁定

Migration 15 为 `app.usage_ledger_entries` 增加必填 `price_version_id` 和 `amount_micros`。写入时数据库同时验证：

- `(price_version_id, token_type)` 对应已定义费率；
- PriceVersion 已发布且不晚于事实的 `observed_at` 生效；
- 非空 Attempt 的 Deployment 与 PriceVersion Deployment 相同；
- 金额是非负、可精确交换的整数 micros。

Usage Ledger 本身只追加，已发布 PriceVersion 与费率也不可变，因此后来发布更便宜或更昂贵的版本不会改变历史请求的版本、币种、单位或已记金额。Request 级无 Attempt 事实仍允许写入，但发布者必须显式提供适用的 PriceVersion；后续用量事件消费负责价格选择与金额计算，数据库负责拒绝不完整或不一致的锁定关系。

## 4. 迁移边界

Migration 15 只允许在 Usage Ledger 为空时升级，因为两个新字段为必填且不能为历史事实猜测价格。Down 会删除 Ledger 的价格锁定字段、全部 PriceVersion/Rate 记录和相关约束，属于有数据丢失风险的开发回滚；生产应采用修复性前滚。
