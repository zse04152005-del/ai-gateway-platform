# Runtime Configuration

`config.Load` 只读取进程级环境配置。Provider、模型、价格、预算和路由属于版本化业务配置，不进入该结构。

本地 `.env` 由启动工具加载，应用本身不隐式搜索文件，避免生产环境意外读取工作目录中的秘密。

## 虚拟 Key 摘要配置

- `VIRTUAL_KEY_HASH_KEY`：32 字节 HMAC 根密钥的 64 位十六进制编码。测试、预发布和生产必须显式配置。
- `VIRTUAL_KEY_HASH_KEY_VERSION`：1～64 位非秘密版本标识，写入 `hash_key_version`，用于后续无停机摘要密钥轮换。
- 仅 `development` 可在显式 Key 为空时，从 32 字节 `LOCAL_ENVELOPE_KEY` 使用 HMAC-SHA-256 和版本化上下文标签派生域分离 Key；其他环境 fail closed。
- 配置加载、错误和日志都不能输出密钥值；调用方获得副本后在构造 Digester 后立即清零临时切片。

## 业务配置 Snapshot

`NewSnapshot` 接受正整数版本、发布时间和单个 JSON 对象，生成不可变快照：

- JSON 先解码再规范化编码，因此空白和对象 Key 顺序不影响 SHA-256。
- 构造输入、`JSON()` 输出和 Store 发布值均复制，调用方不能通过共享 `[]byte` 修改已发布事实。
- `secret_ref` 等引用允许进入快照；`api_key`、`provider_key`、`password`、`authorization`、明文 `token`/`secret` 等字段在任意嵌套层级被拒绝，错误只报告字段路径而不回显值。
- `Decode` 用于把规范 JSON 解码到特定阶段定义的只读业务结构。

`SnapshotStore` 同时实现 `SnapshotReader` 与 `SnapshotPublisher`：

- `Current()` 使用原子指针进行无锁热路径读取。
- `Publish()` 只允许版本前进；相同版本/校验和是幂等重放，相同版本/不同内容是冲突，较低版本是 stale。
- `WaitForVersion(ctx, afterVersion)` 在有更高版本时立即返回，否则等待发布通知或取消，不进行轮询。
- Store 的零值和 `NewSnapshotStore()` 都可安全使用。

PostgreSQL 加载、发布事务、通知丢失恢复和回滚属于 P11；本阶段只提供不会被调用方篡改的进程内热快照契约。
