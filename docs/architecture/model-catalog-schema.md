# Provider、LogicalModel 与 Deployment 目录 Schema

> 状态：Implemented
> 日期：2026-07-30
> 对应任务：P04-T05
> 迁移：`000004_create_model_catalog`

## 1. 目标

模型目录解决三个经常被混在一起的问题：

1. 客户端需要稳定名称，不能因供应商模型改名、区域切换或灰度发布而修改代码。
2. 路由需要知道真实物理端点、区域和能力，不能只比较一个营销模型名。
3. 企业策略需要在绑定阶段发现能力和数据驻留冲突，不能等线上请求失败后才暴露。

因此数据模型明确分为：

```text
Tenant -> LogicalModel -> capability-checked Binding -> Deployment -> Provider
             稳定契约                                  物理事实       协议家族
```

## 2. 实体边界

### 2.1 Provider

Provider 是平台级协议适配器家族，保存：

- 全局唯一 `code`；
- 人类可读名称；
- 可扩展 `adapter_type`；
- active/disabled 状态、乐观版本和审计字段。

Provider 不保存 API Key、Password、Token、Ciphertext 或可恢复凭据。供应商凭据引用由 P04-T07 的独立密钥边界实现。

### 2.2 LogicalModel

LogicalModel 是租户内稳定的客户端模型名，例如 `general-chat`。它保存：

- `tenant_id + name` 唯一标识；
- 展示名和可选说明；
- `required_capabilities` 最低能力契约；
- 可选 `allowed_regions` 数据驻留范围；
- active/disabled、乐观版本和审计字段。

名称必须是规范小写标识符，避免 `/v1/models`、Key 白名单和路由查询出现大小写歧义。

### 2.3 Deployment

Deployment 是可实际调用的物理事实，保存：

- Provider 与 Provider 内唯一代码；
- 供应商真实模型名；
- Endpoint URL；
- Region；
- 完整 `capabilities`；
- active/disabled、乐观版本和审计字段。

Endpoint 此阶段只做结构校验：必须是 HTTP(S) 绝对地址，并禁止 UserInfo、Query、Fragment、空白和控制字符。DNS/IP、重定向、重绑定与出站白名单由 P12-T06 的 SSRF 防护执行；数据库注释明确禁止把语法校验误当安全批准。

### 2.4 LogicalModelDeployment

Binding 使用 `(logical_model_id, deployment_id)` 主键，并保存 priority、weight、状态、版本和审计字段。当前 priority/weight 只是后续路由输入；P06 才实现选择算法。

## 3. Capability Contract

LogicalModel 示例：

```json
{
  "chat": true,
  "stream": true,
  "tools": true,
  "min_context_tokens": 32000,
  "min_output_tokens": 4096,
  "data_retention_modes": ["zero_retention", "self_hosted"]
}
```

Deployment 示例：

```json
{
  "chat": true,
  "stream": true,
  "tools": true,
  "structured_output": true,
  "max_context_tokens": 128000,
  "max_output_tokens": 8192,
  "usage_in_stream": true,
  "cache_usage": true,
  "reasoning_usage": false,
  "data_retention_mode": "zero_retention",
  "provider_protocol_version": "openai-chat-v1"
}
```

数据库和 Go 领域层都执行以下规则：

- 未知 JSON Key 被拒绝，避免拼写错误静默降级。
- Requirement 布尔值只能表达 `true` 的最低要求；禁止用 `false` 混淆“不要求”和“明确禁止”。
- `parallel_tools` 依赖 `tools`，`usage_in_stream` 依赖 `stream`。
- Token 上限必须是正整数，Output 不得大于 Context。
- Data Retention 统一为 `provider_default`、`no_training`、`zero_retention`、`self_hosted`。
- Provider 协议版本是有限、规范化标识符，不接受任意对象扩展。

## 4. 绑定与更新不变量

`app.catalog_deployment_satisfies` 同时检查：

- LogicalModel 要求的每项布尔能力；
- 最小 Context/Output Token；
- 可接受的数据保留模式；
- Deployment Region 是否在允许列表。

三个触发器防止不变量被绕过：

1. 新增或替换 Binding 时，物理 Deployment 必须满足契约。
2. 修改 LogicalModel 要求时，不能让已有 Binding 失效。
3. 修改 Deployment 能力或 Region 时，不能让已有 Binding 失效。

这解决了“创建时校验通过，后来原地编辑导致线上配置悄悄失真”的常见目录漂移问题。若要进行不兼容变更，应先禁用/替换 Binding，再发布新记录或新版本。

## 5. 租户与安全边界

- LogicalModel 通过外键归属 Tenant，并以 `(tenant_id, name)` 唯一。
- Provider/Deployment 是平台目录，可被多个租户的 LogicalModel 安全引用；租户可见性仍由 Binding 查询和后续项目/Key 白名单决定。
- 数据面查询必须从可信 Principal 的 `tenant_id` 开始，不允许直接按客户端提供的 LogicalModel ID 查询。
- 目录四张表不含供应商凭据列；Schema 集成测试显式扫描并拒绝常见凭据列名。
- 删除采用 `RESTRICT`，避免删除 Provider/Deployment 时破坏历史映射。

## 6. 索引与查询方向

- `logical_models(tenant_id, status, name, id)`：支持租户模型列表和名称解析。
- `deployments(provider_id, status, region, code, id)`：支持 Provider/区域运维查询。
- `logical_model_deployments(logical_model_id, status, priority, deployment_id)`：支持确定性候选读取。

P04-T06 将在这些约束之上实现 Key/项目白名单与 `/v1/models`，P06 再加入健康、优先级、权重和可解释路由。

## 7. 验证证据

`TestModelCatalogSchemaConstraints` 覆盖：

- 逻辑名与物理模型/Provider 分离查询；
- 同租户逻辑名唯一、跨租户同名合法；
- 未知能力、重复区域和带 UserInfo Endpoint 被拒绝；
- 缺能力、Region 不匹配的 Binding 被拒绝；
- 已绑定后的 LogicalModel/Deployment 不兼容更新被拒绝；
- 目录表不存在明文凭据字段；
- 迁移 `4→3→4` 可控回滚并恢复。
