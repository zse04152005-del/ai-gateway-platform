# providersecret

Provider 凭据引用与解析边界：

- `local_envelope` 只允许本地开发，使用版本化 AES-256-GCM；Reference ID、Provider ID、名称和 Key Version 作为 AAD，防止密文行被替换复用。
- PostgreSQL 只保存 Ciphertext、12 字节 Nonce 和非秘密 Key Version；Record 与 Metadata 的 JSON 都不暴露 Envelope。
- `vault://`、`kms://` 记录只保存内部 Locator；`ExternalResolver` 为正式 Vault/KMS Adapter 预留接口。
- Reference 与 Deployment 通过 `(provider_id, secret_reference_id)` 复合外键绑定，不能把一个 Provider 的凭据挂到另一个 Provider。
- `Manager.Resolve` 只在构建上游请求的最短边界返回 `[]byte`；调用方用后立即 `clear`，不得转为日志、错误、Metric Label、Trace Attribute、Fixture 或 API 响应。
- 解密失败、缺少历史 Key 和外部后端错误使用稳定哨兵错误，不回显 Locator、Ciphertext、Key Version 或 Provider Secret。

生产环境不得使用 `LOCAL_ENVELOPE_KEY`。生产 Provider Secret 由 Vault/KMS Adapter 解析，数据库仍只保存引用。
