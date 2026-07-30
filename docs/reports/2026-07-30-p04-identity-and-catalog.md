# P04 身份、租户与模型目录验收报告

> 结论：通过
>
> 日期：2026-07-30
>
> 最终远程门禁：[GitHub Actions 30552019583](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30552019583)

## 1. 交付范围

| 任务 | 主要交付 | 提交 | 远程 CI |
|---|---|---|---|
| P04-T01 | Tenant/Project 隔离根与数据库约束 | `62f7485` | `30537121077` |
| P04-T02 | 无可恢复明文的 Virtual API Key Schema | `5adc7dd`、`4a7cbc6` | `30538894748` |
| P04-T03 | Key 签发、轮换、吊销与派生过期 | `0752ed3` | `30541573492` |
| P04-T04 | 数据面 Key 认证、可信 Principal 与有界缓存 | `c516531` | `30543494188` |
| P04-T05 | Provider/LogicalModel/Deployment 与能力契约 | `3a4143f` | `30545430656` |
| P04-T06 | Project/Key 模型白名单与 `/v1/models` | `423a1e9` | `30547514178` |
| P04-T07 | AES-GCM/Vault/KMS Provider Secret Reference | `9e0a6ad` | `30549664128` |
| P04-T08 | 两租户隔离回归矩阵与缓存前缀绑定 | `b5f7de0` | `30552019583` |

## 2. 阶段门禁证据

### Virtual Key 生命周期完整

- 完整凭据由 CSPRNG 生成，只在 Create/Rotate 成功边界返回一次；数据库仅保存安全前缀、版本化 HMAC-SHA-256 摘要与授权元数据。
- Tenant/Project 复合外键、同域轮换来源、全局前缀/摘要唯一和生命周期 CHECK 在 PostgreSQL 强制执行。
- 轮换持有源行锁并以版本条件更新，并发测试证明只产生一个替代者；旧 Key 只在明确宽限期内可用。
- 吊销幂等，绝对过期与轮换宽限在每次认证时按当前时间重新判断；状态失效支持缓存前缀主动清除。
- 缺失、错误、未知、吊销、过期、Tenant/Project 禁用统一为 `401 INVALID_API_KEY`，数据库/Keyring 不可信时 fail closed 为可重试 503。

### 模型目录与能力契约可用

- LogicalModel 是 Tenant 范围内稳定名称，Deployment 是 Provider 物理端点，Binding 将两者解耦。
- Capability Contract 覆盖 Chat、Stream、Tools、Structured Output、Vision、Audio、Embedding、Token 上限、Usage 与 Data Retention。
- PostgreSQL 触发器在绑定和后续模型/部署更新时再次验证能力、Region 和 Data Retention，阻止兼容性漂移。
- Project 白名单与 Key 的 nil/空/非空三态收窄形成授权交集；`/v1/models` 只返回 active 且物理可用的逻辑模型和客户端能力。
- 响应不包含 Provider、Deployment、物理模型、Endpoint 或其他租户标识。

### 多租户越权测试通过

- 两个完整 Tenant/Project 使用不同 Key、模型和 Provider 链路，在同一真实 PostgreSQL 中相互对抗。
- Key 直接 ID 的 Get/Rotate/Revoke 同时要求 Tenant/Project/ID；跨域与随机不存在返回相同错误且源 Key 不变。
- A Tenant+B Project 以及反向组合的目录查询均返回空集合；跨租户 Project→Model 授权由复合外键拒绝。
- 认证缓存写入要求缓存键等于 Record 的全局唯一安全前缀，输入、输出和 Principal 对可变策略深拷贝；伪造身份 Header 无法覆盖数据库作用域。
- Provider Secret 按 Provider+Reference 查询，Deployment 复合外键拒绝跨 Provider Reference；外部错误和明文不进入 API、日志或 Metadata。
- 错误 Secret 与未知前缀的公开 HTTP 状态、错误码、消息与结构完全一致，不形成存在性 Oracle。

完整矩阵见 [`docs/security/tenant-isolation.md`](../security/tenant-isolation.md)。

## 3. 测试与质量结果

- 工作副本与桌面正式仓库均通过模块校验、格式、vet、普通/Integration lint、全量单测、构建、govulncheck、迁移顺序、Actionlint 和高风险凭据扫描。
- 每个数据库任务均使用精确命名的可抛弃 PostgreSQL 数据库；最终全历史回归包含 P04-T01～T08 所有 Schema/生命周期/认证/目录/Secret/隔离用例。
- 最新迁移数为 6；重复 Up 幂等，生产环境 Down 正确拒绝，开发验证完成 `6→5→6`，临时数据库最终存在数为 0。
- 最终 Linux CI 对普通单元执行 race detector，对真实 PostgreSQL 执行全历史迁移和 P04 回归，并通过 YAML、本地 Secret 位置、高风险模式与 Gitleaks 扫描。
- Gitleaks 的 Node.js 20→24 弃用提示不是失败；P04-T08 的合成 Fixture 误报已从最终 Git 历史移除，未扩大精确忽略列表。

## 4. 已知边界与后续映射

- 当前跨进程认证缓存失效总线尚未接入；主动失效不可用时由短 TTL 限定残余窗口，后续事件总线阶段补齐。
- 当前目录只负责列出可用逻辑模型，尚未实现请求路由、上游调用与健康选择；P05～P08 依次交付 Mock/Adapter、非流式、流式与路由策略。
- 数据库隔离依赖复合约束与 Repository 谓词，尚未启用 PostgreSQL RLS；新增 Repository 必须扩展隔离矩阵，高敏感管理查询可在后续评估 RLS。
- Provider Secret 已有安全引用和本地开发 Envelope，但生产部署必须使用 Vault/KMS Resolver，禁止配置本地 Envelope Key。

这些边界均有明确后续任务，不阻塞 P04 阶段门禁。
