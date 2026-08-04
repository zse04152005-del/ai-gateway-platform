# P10-T03 价格版本模型验收报告

- 日期：2026-08-03
- 范围：生效时间、区域、币种、计费单位、Token 类型与历史请求价格锁定
- 结论：实现、本机完整门禁与 GitHub Actions 三个 Job 全部通过

## 1. 实现结果

新增 `internal/metering` PriceVersion/PriceRate 领域模型和 Migration 15。PriceVersion 绑定 Deployment、Region、三位大写 Currency 与 `effective_at`，按 `draft/version=1 → published/version=2` 单向发布；发布后身份、生命周期和费率均不可原地修改。

普通输入/输出、缓存和推理只接受 Token；音频接受 Token 或秒；图像接受 Token 或图像。每项费率保存报价单位数量与整数 micros 单价，未知 Token 类型、不兼容单位和超出精确整数范围的值均 fail closed。

## 2. 历史锁定与并发边界

Usage Ledger 新增必填 `price_version_id` 和 `amount_micros`，并以 `(price_version_id, token_type)` 复合外键锁定具体费率。写入触发器拒绝 draft、尚未生效和 Attempt Deployment 不匹配的版本；Ledger、已发布版本和费率共同不可变，后续价格发布不会改写历史事实。

费率追加会锁定父 PriceVersion，使它与并发发布严格串行：先完成的费率属于发布快照，发布完成后到达的费率被拒绝。Migration 15 在升级前要求现有 Usage Ledger 为空，避免为历史请求猜测价格。

## 3. 场景覆盖

- Go 领域模型的 draft/published 生命周期、生效时间和历史费率选择；
- 九种 Token 类型的 Token/秒/图像单位兼容矩阵与精确整数边界；
- PostgreSQL Deployment/Region 外键、Currency 格式、单一发布时间版本和发布前至少一条费率；
- 未发布、未生效、缺少 Token 费率和 Attempt Deployment 错配的 Usage 拒绝；
- 历史 Ledger 的 PriceVersion/金额锁定，以及版本、费率和 Ledger 的 UPDATE/DELETE 拒绝。

## 4. 门禁结果

- `go test -count=20 -cover ./internal/metering`：连续 20 轮通过，覆盖率 100.0%；
- 真实 PostgreSQL PriceVersion/Usage Ledger/Taxonomy 专项：连续 20 轮通过；
- `scripts/dev.ps1 -Action test-integration`：完整 PostgreSQL/Redis/进程集成套件通过；
- `scripts/dev.ps1 -Action check`：模块校验、格式、双标签 lint、全量单测、构建、govulncheck、15 个迁移顺序、Actions 语法和本地密钥扫描全部通过；
- 本机迁移生命周期 `15→14→15` 已获单独授权并通过，两端均为 `dirty=false`；恢复后的 PriceVersion/Usage Ledger/Taxonomy 回归再次通过。

## 5. 远端证据

实现提交为 `460f2f5`。GitHub Actions [`30805483073`](https://github.com/zse04152005-del/ai-gateway-platform/actions/runs/30805483073) 的 `go-quality`、`migration-integration`、`config-and-secrets` 三个 Job 全绿；Linux race、Migration `15→14→15`、真实 PostgreSQL PriceVersion 历史锁定、漏洞和密钥扫描均明确通过。
