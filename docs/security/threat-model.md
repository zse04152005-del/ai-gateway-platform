# 威胁模型

> 状态：Initial Baseline  
> 日期：2026-07-30  
> 对应任务：P01-T03

## 1. 范围

覆盖 Gateway、Control Plane、Metering Worker、PostgreSQL、Redis、事件总线、ClickHouse、遥测链路以及外部 Provider 接口。P2 的 MCP/A2A 会在实施前扩展本模型。

## 2. 关键资产

- 虚拟 API Key 与供应商凭证。
- Prompt、Response、工具 Schema 和业务元数据。
- 租户、项目、模型权限和区域策略。
- 预算、价格、Usage Ledger 与对账信息。
- 路由策略、配置快照和审计日志。
- 系统容量、连接、队列和可用性。

## 3. 攻击者

- 持有合法 Key 但尝试越权的租户用户。
- 泄漏 Key 的外部攻击者。
- 恶意或被攻陷的 Provider/Endpoint。
- 能提交配置但权限有限的内部用户。
- 通过 Prompt、Header、SSE Chunk、Webhook 或账单文件输入恶意数据的主体。
- 依赖或构建链中的供应链攻击者。

## 4. STRIDE 风险清单

| ID | 类别 | 风险 | 等级 | 缓解 | 验证 |
|---|---|---|---|---|---|
| T-01 | Spoofing | 猜测、盗用或重放虚拟 Key | 高 | 强随机 Key、版本化 HMAC、常量时间比较、到期、轮换、统一 401 | Key 生命周期、认证与重放测试 |
| T-02 | Tampering | 修改路由、价格或预算逃避治理 | 高 | 管理 RBAC、版本发布、审计、乐观锁 | 越权与并发更新测试 |
| T-03 | Repudiation | 管理员否认配置/导出操作 | 高 | 追加审计、actor、reason、before/after、traceId | 审计完整性检查 |
| T-04 | Information Disclosure | Key/Prompt 进入日志、Trace、错误 | 严重 | 默认不存正文、统一脱敏、日志测试 | 全仓/日志敏感模式扫描 |
| T-05 | Information Disclosure | 跨租户读模型、用量或缓存 | 严重 | 数据库 Principal、删除客户端身份 Header、Repository 强制过滤、有界缓存与显式失效 | 认证伪造 Header、缓存失效与跨租户测试矩阵 |
| T-06 | Elevation | 数据面 Key 调用管理 API | 严重 | 管理面独立身份与受众、路由隔离 | Token audience/role 测试 |
| T-07 | SSRF | 管理员配置恶意 Provider 地址 | 严重 | 域名白名单、DNS/IP 校验、禁重定向、出站策略 | 回环/元数据/DNS 重绑定测试 |
| T-08 | DoS | 超大正文、Header、SSE Chunk 或慢客户端 | 高 | 大小限制、有界缓冲、写超时、连接/并发限制 | 资源压力与慢客户端测试 |
| T-09 | DoS | 重试风暴放大 Provider 故障 | 高 | Attempt/时间预算、熔断、抖动、首包后不重试 | 故障风暴测试 |
| T-10 | Tampering | 重复用量事件造成重复计费 | 高 | eventId 唯一、幂等消费、不可变 Ledger | 事件重复 10 次测试 |
| T-11 | Elevation | 并发请求绕过预算 | 严重 | 原子预留、多维账本、过期回收 | 100+ 并发硬预算测试 |
| T-12 | Information Disclosure | 遥测高基数或正文泄漏 | 高 | 遥测字段白名单、Label 审查、采样策略 | Metric/Span 自动检查 |
| T-13 | Tampering | 恶意 Provider 返回异常流/usage | 高 | SSE/JSON 限制、Adapter 契约、未知字段隔离 | Fuzz/Fixture 测试 |
| T-14 | Supply Chain | 依赖或镜像被篡改 | 高 | 固定版本、漏洞扫描、SBOM、镜像签名 | CI 安全门禁 |
| T-15 | Repudiation | 预算或账单修正覆盖历史 | 高 | Adjustment 分录，不直接更新旧 Ledger | 数据库约束与审计测试 |

## 5. 安全设计要求

### 5.1 身份与租户

- 数据面虚拟 Key 与管理面 OIDC Token 使用不同验证器和 audience。
- tenantId/projectId 从认证上下文得出，不信任请求参数。
- 缺失、格式错误、未知、错误 Secret、吊销、过期和作用域禁用对外使用同一 401，不提供 Key 枚举 Oracle。
- 认证正缓存只存摘要与作用域，支持按前缀失效；绝对过期/轮换宽限每次重新判断，TTL 是主动失效故障时的明确残余风险上界。
- 导出、审计和明细请求使用更细权限。

### 5.2 密钥

- 虚拟 Key 只存哈希和前缀。
- Provider Key 使用可替换的 KMS/Vault 信封加密接口。
- 本地开发 Envelope 使用版本化 AES-256-GCM 与 Reference/Provider/Name AAD；生产禁止本地 Key，Deployment 复合外键禁止跨 Provider 引用。
- Provider Secret 明文只在加密输入或上游请求构造的最短 `[]byte` 边界存在，用后清零；Locator、Ciphertext 和外部错误也不进入公开输出或遥测。
- Key 不写入错误、日志、Trace、Fixture、命令历史和配置快照明文。

### 5.3 内容

- 默认不保存 Prompt/Response。
- 内容留存必须有租户策略、保留期限、用途、访问角色和审计。
- 代理响应中的敏感 Header 不向客户端透传。

### 5.4 网络

- 只允许 HTTPS 或明确批准的本地 Mock。
- 禁止访问回环、链路本地、RFC1918（除批准私有集群）和云元数据地址。
- 默认禁用跨域重定向与自动携带凭证。

## 6. 零容忍事件

- 供应商 Key、虚拟 Key 或 Prompt/Response 明文泄漏。
- 跨租户访问或缓存污染。
- 硬预算被未授权请求突破。
- 重复有效计费分录。
- 未审计的价格、预算或路由生产变更。

发生以上事件时停止功能发布，保留证据，修复并增加回归测试。

## 7. 剩余风险

- Provider 是否实际停止生成无法完全由网关保证；需记录取消请求与上游结束证据。
- Tokenizer 估算与供应商账单可能不同；必须标记来源并通过 P1 对账修正。
- 供应商可能在相同模型别名下改变行为；P1 使用版本治理和评测门禁降低风险。
