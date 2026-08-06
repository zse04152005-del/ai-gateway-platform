# meteringadjustment

P10-T08 的追加式 Usage Ledger 修正边界。调用方提供可信 Tenant/Project Scope、稳定 Event ID 与幂等键、原始 Ledger Event ID、绝对修正后数量/金额，以及有限来源、原因码、外部证据引用和操作者身份。

Writer 锁定原始非 Adjustment 行，读取它的全部既有修正，计算本次 signed delta 后追加新行。原始行与历史修正继续由数据库拒绝 UPDATE/DELETE；并发重放只创建一行，同一幂等键携带不同事实返回冲突。数据库同时强制修正与原始行属于同一 Tenant、Request、Attempt、Token 类型和 PriceVersion，禁止 Adjustment 引用 Adjustment，也禁止修正后的数量或金额为负。

审计字段只保存有限标识符，不保存账单文件、Prompt、Response、Credential 或自由文本。P11 对账导入可把批次/条目摘要作为 `reference`，但原始外部证据必须留在受控证据存储中。
