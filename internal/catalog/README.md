# catalog

`catalog` 定义 Provider、LogicalModel、Deployment 与 Capability Contract 的纯领域模型，不依赖 HTTP、PostgreSQL、供应商 SDK 或密钥系统。

核心边界：

- Provider 只标识协议适配器家族，不保存凭据。
- LogicalModel 是租户内稳定的客户端名称，只声明最低能力和允许区域。
- Deployment 是供应商物理模型、Endpoint、区域和实际能力集合。
- Binding 只连接满足逻辑模型能力/驻留要求的 Deployment；数据库触发器也执行同一不变量。
- `Satisfies` 用于后续路由候选过滤，不能替代 Provider 健康、预算、白名单或 SSRF 检查。
- Endpoint 在本模块只做结构校验；DNS/IP、重定向和出站目的地安全由 P12-T06 的安全层负责。
- Provider 凭据引用由 P04-T07 独立实现，避免把明文或可恢复 Secret 混入目录记录。
