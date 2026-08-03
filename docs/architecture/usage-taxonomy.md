# Usage Token Types and Sources

> 状态：Implemented
>
> 日期：2026-08-03
>
> 对应任务：P10-T02

## 1. 有限 Token 类型

Usage Ledger 只接受以下独立计量维度：

| Token 类型 | 含义 |
|---|---|
| `input` | 普通输入 Token |
| `output` | 普通输出 Token |
| `cache_read` | 缓存读取/命中计量 |
| `cache_write` | 缓存写入/创建计量 |
| `reasoning` | 供应商明确报告的推理计量 |
| `audio_input` | 输入音频计量 |
| `audio_output` | 输出音频计量 |
| `image_input` | 输入图像计量 |
| `image_output` | 输出图像计量 |

这些维度不能机械相加：Cache Read 可能属于 Input 的子集，也可能是独立 Meter；Reasoning 可能属于 Output 的子集。当前 Ledger 只保存原子事实，P10-T03 的 PriceVersion 决定每个维度采用 Token、图像或其他单位及其价格。

`total`、`audio`、`image` 和供应商自定义字段不是合法类型。总量是查询投影而不是可重复计费的分录；音频与图像必须保留输入/输出方向；未知 Provider Meter 留在受限 `adapter.UsageEvidence`，在映射发布前不能按已知类型收费。

## 2. 有限来源

| 来源 | 语义 |
|---|---|
| `provider` | Provider 在响应或流中报告的原始事实，Adapter 边界要求 Raw Evidence |
| `estimated` | 本地版本化估算器产生，不能冒充 Provider 精确值 |
| `reconciled` | 供应商账单或可信异步回填产生的对账事实 |
| `adjustment` | 对历史不可变分录的显式修正；P10-T08 将要求引用原分录和操作者 |

来源描述“谁产生事实”，不是精度排序、Token 类型或幂等键。Reconciled/Adjustment 也只能追加新分录，不能 UPDATE 原历史。

## 3. 单一契约与数据库一致性

`internal/metering` 定义 Token 类型，并直接复用 `adapter.UsageSource` 的四个常量，避免 Adapter 与 Metering 维护两套来源。解析严格区分大小写且不自动 trim；不可信字符串必须显式失败。

Migration 14 把 Migration 13 的通用安全格式约束收紧为同一有限集合。新约束先 `NOT VALID` 加入，立即约束新写入，再在同一事务内验证全部历史行；存在未知旧值时迁移 fail closed，不会静默重分类。真实 PostgreSQL 测试写入 9×4 全部合法组合，并证明 Go 接受而数据库遗漏、或数据库接受而 Go 未定义的漂移不会通过 CI。
