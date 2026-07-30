# Provider Secret Reference 与信封加密

> 状态：Implemented
> 日期：2026-07-30
> 对应任务：P04-T07
> 迁移：`000006_create_provider_secret_references`

## 1. 目标与非目标

本模块让 Deployment 引用 Provider 凭据，但不让凭据明文进入目录、API、日志、错误、配置快照或 PostgreSQL。

当前实现包含：

- development-only 的版本化 AES-256-GCM 本地信封加密；
- Vault/KMS Resolver 端口；
- Provider 绑定的 Secret Reference Schema；
- 安全 Metadata 和最短明文解析边界；
- 跨 Provider 引用防护。

本阶段不提供管理 HTTP API；P11 的管理 API 只能复用本模块，不能自行保存或返回 Secret。P12 会补充审计、RBAC 与运行态安全门禁。

## 2. 数据模型

`app.provider_secret_references` 的 backend 有三种：

| Backend | PostgreSQL 保存 | PostgreSQL 不保存 |
|---|---|---|
| `local_envelope` | Ciphertext、12-byte Nonce、Key Version | Locator、Plaintext |
| `vault` | `vault://` 内部 Locator | Ciphertext、Nonce、Key Version、Plaintext |
| `kms` | `kms://` 内部 Locator | Ciphertext、Nonce、Key Version、Plaintext |

数据库 CHECK 强制 backend 与材料互斥，不能同时出现本地 Ciphertext 和外部 Locator。Locator 禁止 UserInfo、Query、空白与控制字符；Locator 本身仍被视为内部敏感元数据，Record JSON、Metadata、日志、错误、Metric 和 Trace 均不得输出。

Deployment 通过以下复合外键引用：

```text
deployments(provider_id, secret_reference_id)
    -> provider_secret_references(provider_id, id)
```

因此知道另一个 Provider 的 Reference UUID 也不能挂载，避免凭据误路由或越权复用。无凭据的本地 Mock 可以保持 `secret_reference_id = NULL`。

## 3. 本地开发信封

`providersecret.LocalCipher` 使用 AES-256-GCM：

- 每次加密从 `crypto/rand` 读取全新 12-byte Nonce；
- 明文限制为 1～16 KiB；
- GCM Tag 与 Ciphertext 一起持久化；
- Reference ID、Provider ID、Reference Name、Key Version 作为 AAD；
- 复制同一密文到另一条 Reference、Provider 或 Name 时认证失败；
- 当前 Key 负责写入，保留 Keyring 只用于解密历史版本；
- 缺少版本、Nonce/Tag 错误或 AAD 不一致统一为 `ErrDecryptionFailed`。

`LOCAL_ENVELOPE_KEY` 只允许 development，必须解码为 32 bytes；`LOCAL_ENVELOPE_KEY_VERSION` 是非秘密版本标识。生产配置本地 Key 会在配置校验阶段 fail closed，生产必须注入 Vault/KMS Adapter。

## 4. Vault/KMS 端口

```go
type ExternalResolver interface {
    Resolve(context.Context, string) ([]byte, error)
}
```

Manager 以 backend 注册 Resolver：

- 没有 Adapter、后端错误、空值或超限值统一返回 `ErrBackendUnavailable`；
- 外部错误不会透传，避免其中包含 Locator 或 Provider Secret；
- Resolver 返回值被复制后立即清零原切片；
- 调用者取得最终 `[]byte` 后，只能用于构造当前上游请求，并应立即 `clear`。

## 5. 明文生命周期

```text
CreateLocal input []byte
  -> 受控副本
  -> AES-GCM Encrypt
  -> 清零受控明文副本
  -> Store 只接收 Envelope

Resolve
  -> provider-scoped Store Get
  -> local Decrypt 或 external Resolver
  -> 上游请求构造
  -> 调用方 clear
```

`Record` 的 Locator、Ciphertext、Nonce、KeyVersion 和 Create Command 的 Plaintext 都标记为 `json:"-"`。`Metadata` 只包含 Reference ID、Provider ID、Name、Backend、Status、Version 和审计字段。

## 6. 失败语义

- `ErrNotFound`：Provider/Reference 组合不存在，不区分具体缺失对象。
- `ErrConflict`：Provider 内 Reference Name/ID 冲突。
- `ErrDisabled`：Reference 已禁用。
- `ErrDecryptionFailed`：本地 Key/Nonce/Tag/AAD 不可信。
- `ErrBackendUnavailable`：Vault/KMS Adapter 不可用或返回值不可信。

这些错误不包含明文、Locator、Ciphertext、Key Version 或外部错误正文。PostgreSQL 错误只在内部被包装，公开 API 在未来管理/代理边界使用固定安全错误定义。

## 7. 验证

单元测试覆盖随机 Nonce、同明文不同 Ciphertext、AAD 替换失败、保留版本解密、Metadata/Record JSON 不泄漏、Vault 错误净化与禁用状态。

`TestProviderSecretEnvelopeAndReferenceIsolation` 使用真实 PostgreSQL 验证：

- 明文不等于且不包含于数据库 Ciphertext；
- Local Envelope 可解密回原值；
- 同 Provider Deployment 绑定成功；
- 跨 Provider 绑定被复合外键拒绝；
- Vault Reference 无 Ciphertext；
- Local/External 混合材料被 CHECK 拒绝；
- 表中不存在 Plaintext/API Key/Password/Raw Secret 列；
- 禁用 Reference 后解析 fail closed；
- 迁移 `6→5→6` 可控回滚恢复。
