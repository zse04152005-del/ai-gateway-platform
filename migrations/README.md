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
- Up 重复执行时，迁移引擎返回 no-change 并以成功退出，不重复运行已登记版本。
- Down 只用于开发与可控回滚，CLI 要求 `--confirm-development`，并在 `APP_ENV=production` 时强制拒绝。
- 含数据丢失风险的回滚必须单独审批；生产优先采用修复性前滚，不能把 Down 当作常规发布手段。
- CI 在临时 PostgreSQL 中执行：顺序校验 -> 空库 up -> 再次 up/no-change -> down 1 -> up，验证新库、幂等和受控回滚。
