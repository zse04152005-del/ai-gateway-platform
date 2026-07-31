# P09-T04 TPM 预估与结算验收报告

- 日期：2026-07-31
- 范围：版本化输入估算、输入加最大输出预留、Redis 四层原子 TPM、原分钟终态结算
- 本地结论：实现与专项门禁通过；远程提交和 GitHub Actions 证据待本任务闭环后回填

## 1. 预估与口径

新增 `InputTokenEstimator`、`TPMReservationPlan` 和 `ActualTPM`。预留量固定为估算输入加请求最大输出；请求没有最大输出时必须由已选择 Deployment 的 Capability 显式提供，限流层不猜默认值。估算结果携带 Method、Version 和 `Estimated=true`，不能冒充 Provider Usage。

内置 `NormalizedJSONByteEstimator` 以深拷贝后的规范化请求 JSON 字节加固定 framing allowance 形成有上限的确定性 fallback，标记为 `normalized-json-byte-bound/v1`。P10-T07 的模型 Tokenizer 上线后可以替换该接口而不改变预留协议。

实际 TPM 默认只使用 Present 的主 `input_tokens + output_tokens`。Cache Read/Write、Reasoning 和 Audio 维度保持独立，不重复计入。Partial/Estimated 的事实不会被改写为 Complete。

## 2. Redis 预留与结算

新增 `RedisTPMLimiter`。Reserve Lua 以 Redis `TIME` 为分钟权威，在任何写入前检查 Platform、Tenant、Project、VirtualKey 四层 hard 和计数；全部允许才统一增加估算量，拒绝无部分写入。Reservation ID、reserved 和四层 tenant-qualified Scope SHA-256 指纹共同防止错误重放，相同状态重试幂等。

Settle Lua 总是引用 Reserve Handle 的原分钟 Key。`actual < reserved` 时四层统一释放差额；`actual > reserved` 时统一补记超额，允许计数超过 hard 以保留真实使用量，并阻止后续 admission。重复相同 actual 幂等，不同 actual/Scope/reserved 冲突，过期 Key、损坏计数、下溢、数值越界和异常 RESP 均 fail closed。

Key 使用版本化 `{tpm}` hash tag。`PEXPIREAT` 固定为原分钟 ResetAt 加 Settlement Retention，默认一小时、最大 24 小时；重试和结算不滑动 TTL。

## 3. 自动化覆盖

- 请求最大输出和 Deployment Capability fallback；估算器方法/版本、深拷贝、上限、取消和错误 fail closed；
- Actual 只合并 Input/Output，缺失、负数、Adjustment 和 `2^53-1` 越界拒绝；
- Redis/本地分钟偏差纠正和三次边界重试上限；
- 四层 canonical Scope、Policy soft/hard、ID、Handle、Scope 指纹与 Options 校验；
- 预留 hard 拒绝无部分计数，协议回显、损坏 Counter、Redis 传输错误 fail closed；
- 差额释放、零差额、实际超额补记、相同终态幂等、不同终态冲突、跨分钟完成引用原 Key；
- 真实 Redis 64 路并发，每次预留 25、hard=500，严格 20 个允许/44 个拒绝；
- 20 个允许项并发结算到实际 10 后，四层从 500 精确变为 200；
- 实际 120 超过预留 80 时四层精确补记到 120，下一次 admission 在 hard=100 下拒绝；
- 真实 Redis 验证 Reservation Marker、绝对 TTL 不续期、过期和损坏状态无旁路修改。

## 4. 本地门禁结果

- `go test -count=20 -cover ./internal/limits`：连续 20 轮通过，合并覆盖率 93.6%；
- `TestRedisTPMAtomicReservationAndSettlement` 在真实 Redis 上连续 10 轮通过；
- `scripts/dev.ps1 -Action test-integration`：含真实 PostgreSQL、Redis 的完整集成套件通过；
- 常规与 integration build tag 的 `go vet`、golangci-lint：0 issue；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、迁移顺序、Actions 语法和本地密钥扫描全部通过，迁移仍为 `count=10 latest=000010_create_limit_policies`。

实现提交、GitHub Actions 三 Job 以及清单证据提交将在后续闭环步骤完成并回填。
