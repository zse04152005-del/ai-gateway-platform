# 安全文档

本目录保存威胁模型、权限矩阵、密钥管理、数据留存、SSRF 防护和安全测试计划。

默认原则：

- 不保存虚拟 Key 或供应商 Key 明文。
- Prompt/Response 默认不进入日志、Trace 和错误响应。
- 所有资源查询强制租户边界。
- 管理配置、预算和审计操作不得由模型输出直接决定。

## 结构化日志安全基线

- 所有进程使用一行一条的 JSON 日志，固定包含 `service`、`version`、`level`、`requestId`、`traceId`、`tenantId`、`projectId`。
- `Authorization`、`Cookie`、`Prompt`、`Response`、Provider/API Key、密码和 Secret 等字段名不区分大小写、分隔符并递归脱敏。
- 未知复杂对象默认整体隐藏；只有调用方明确拆分出的低基数字段才允许写入。
- 启停日志只记录监听地址、Broker 数量等安全元数据，不记录数据库 URL、Broker 地址或密钥。
- Bootstrap 失败仅记录稳定错误码；详细内部错误保留在进程返回链路，不直接写入生产日志。
- 调用方不得把 Prompt、Response、完整请求/响应对象或第三方错误体拼进日志消息文本；字段脱敏不能保护自由文本。
