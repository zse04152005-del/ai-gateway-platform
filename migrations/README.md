# 数据库迁移

项目内置 `go run ./cmd/migrate` 命令，底层使用固定版本的 `golang-migrate/migrate`，SQL 文件采用标准格式：

- `NNNNNN_description.up.sql`
- `NNNNNN_description.down.sql`

## 命令

```powershell
# 不连接数据库，CI 和本地均可执行
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action migrate-validate

# 从 .env 读取 DATABASE_URL
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action migrate-up
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action migrate-version

# 仅开发环境；必须明确选择 down 动作
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action migrate-down
```

也可先导出 `DATABASE_URL`，再执行 `go run ./cmd/migrate up|down|version`。命令不会输出连接串。

## 文件与顺序规则

- 版本从 `000001` 开始且必须连续递增，不允许缺号、重复版本或同版本不同名称。
- 每个版本必须同时存在非空的 up/down 文件；迁移目录必须保持扁平。
- `go run ./cmd/migrate validate --path migrations` 不需要数据库，CI 用它阻止错误顺序进入主分支。
- 已应用迁移不修改，只追加新迁移。
- 生产变化遵循 expand/migrate/contract。

## 基线与回滚约定

- `000001_create_app_schemas` 是空库基线，只创建 `app` 和 `audit` Schema，不创建业务表。
- `000002_create_tenants_projects` 创建隔离根、状态/配额引用/乐观锁/审计字段，以及租户内唯一约束和查询索引。
- `000003_create_virtual_api_keys` 只保存非秘密前缀和 32 字节 keyed digest，使用租户/项目复合外键，并约束白名单、限额和轮换/吊销生命周期。
- `000004_create_model_catalog` 分离 Provider、租户 LogicalModel、物理 Deployment 和 Binding；严格校验能力/区域 JSON，并以触发器阻止不兼容绑定及后续配置漂移。
- `000005_create_project_model_allowlist` 用 Tenant 复合外键建立 Project 到 LogicalModel 的安全授权关系，Key 白名单只能在查询时进一步收窄。
- `000006_create_provider_secret_references` 保存 Provider 绑定的本地认证密文或 Vault/KMS Locator，并以复合外键阻止 Deployment 跨 Provider 引用凭据。
- `000007_create_gateway_execution_records` 保存无内容的 GatewayRequest、每次物理调用独立 RouteAttempt 和追加式状态事件，并由触发器强制合法状态迁移与乐观版本单调。
- `000008_allow_unknown_gateway_logical_model` 允许先记录客户端请求的未知模型名，再由路由层返回 `MODEL_UNAVAILABLE`；避免可变目录外键把可审计的业务失败误报成记录服务故障。
- `000009_create_route_decisions` 按 Request 保存每次候选过滤、路由评分、重试判定与最终 Deployment 的无内容解释事实；决策序号可复盘故障切换中的每次重选。
- `000010_create_limit_policies` 创建 Tenant 内版本化的 RPM/TPM/并发 soft/hard 稀疏策略，并为 Tenant/Project/Key 增加阻止跨租户绑定的强类型引用；旧引用仅在 expand 阶段兼容保留且不能与新引用共存。
- `000011_create_budget_ledger` 创建 Tenant/Project/Key/User/Session 独立预算账户、幂等预留和只追加账本；金额统一为整数 micros，复合外键隔离租户，触发器固定账户/预留单向生命周期与账本不可变性。
- `000012_allow_closed_budget_settlement` 保持账户身份、限额和关闭状态不可逆，同时允许关闭后以 version+1 结算仍在途的预留，避免周期关闭吞掉真实费用。
- `000013_create_usage_ledger` 在既有 Request/Attempt 事实根上创建只追加 Usage Ledger，以全局唯一 `event_id` 幂等、复合外键固定 Tenant/Request/Attempt 归属，并允许缓存等 Request 级事实不绑定物理 Attempt。
- `000014_constrain_usage_taxonomy` 将 Ledger Token 类型收紧为九个独立输入/输出/缓存/推理/音频/图像维度，并将来源收紧为 provider/estimated/reconciled/adjustment；未知历史值会阻止约束验证而不会被静默归类。
- Up 重复执行时，迁移引擎返回 no-change 并以成功退出，不重复运行已登记版本。
- Down 只用于开发与可控回滚，CLI 要求 `--confirm-development`，并在 `APP_ENV=production` 时强制拒绝。
- 含数据丢失风险的回滚必须单独审批；生产优先采用修复性前滚，不能把 Down 当作常规发布手段。
- CI 在临时 PostgreSQL 中执行：顺序校验 -> 空库 up -> 再次 up/no-change -> Schema 约束测试 -> down 1 -> up，验证新库、幂等和受控回滚。
