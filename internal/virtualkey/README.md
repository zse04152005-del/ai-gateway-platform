# virtualkey

虚拟数据面凭据的领域与持久化模块，不依赖 HTTP：

- `Manager` 校验 Tenant/Project/Actor、模型白名单、限额、过期和宽限期，并协调一次性签发。
- `HMACDigester` 使用版本化 32 字节根密钥计算 `HMAC-SHA-256(prefix || 0x00 || secret)`。
- `PostgresStore` 的每个查询都带 `tenant_id + project_id + id`；轮换和吊销使用行锁与乐观版本条件。
- `Record.SecretHash` 显式禁止 JSON 序列化；`Metadata` 不含摘要，`IssuedCredential` 仅用于创建/轮换返回边界。

完整凭据格式为安全前缀、点号和 32 字节 base64url Secret。前缀可进入审计和支持流程，完整凭据与 Secret 不得进入日志、错误、指标标签、Trace、数据库或消息事件。

过期状态按调用方时钟从 `expires_at` 派生；轮换旧 Key 在宽限截止后派生为 `rotation_grace_elapsed`。这两种状态不依赖定时任务修改事实行，避免 Worker 延迟导致权限窗口延长。
