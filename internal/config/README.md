# Runtime Configuration

`config.Load` 只读取进程级环境配置。Provider、模型、价格、预算和路由属于版本化业务配置，不进入该结构。

本地 `.env` 由启动工具加载，应用本身不隐式搜索文件，避免生产环境意外读取工作目录中的秘密。

## 上游 HTTP 连接池

Gateway 只在进程启动时创建一个共享 Provider HTTP Client。`UPSTREAM_HTTP_CONNECT_TIMEOUT`、`UPSTREAM_HTTP_TLS_HANDSHAKE_TIMEOUT`、`UPSTREAM_HTTP_RESPONSE_HEADER_TIMEOUT`、`UPSTREAM_HTTP_TOTAL_TIMEOUT`、`UPSTREAM_HTTP_IDLE_CONN_TIMEOUT` 与 `UPSTREAM_HTTP_EXPECT_CONTINUE_TIMEOUT` 分别控制连接、TLS、首部、非流式总时长、空闲复用和 100-continue 等待；非法值在监听前 fail closed。

`UPSTREAM_HTTP_MAX_IDLE_CONNS`、`UPSTREAM_HTTP_MAX_IDLE_CONNS_PER_HOST` 和 `UPSTREAM_HTTP_MAX_CONNS_PER_HOST` 控制全局/单 Provider 连接池，后两者不能形成“空闲上限大于总连接上限”的无效组合。`UPSTREAM_HTTP_MAX_RESPONSE_HEADER_BYTES` 限制 Provider 响应头为 1 KiB～1 MiB。默认值见 `.env.example`，安全与复用语义见 [`internal/upstreamhttp`](../upstreamhttp/README.md)。

## Mock Provider 最小配置

`config.LoadMockProvider` 只读取环境、日志、`MOCK_PROVIDER_HTTP_ADDR` 和共享 HTTP 超时，不要求任何数据库、缓存、消息、分析或遥测地址。它只允许 development/test，并强制 Loopback 监听；staging/production 或公网地址会在打开 Listener 前 fail closed。

## 本地 Provider 信封加密

- `LOCAL_ENVELOPE_KEY`：仅 development 可用的 32 字节 AES-256-GCM Key（64 位十六进制）；生产环境配置该值会 fail closed。
- `LOCAL_ENVELOPE_KEY_VERSION`：1～64 位非秘密版本标识，写入本地开发 Envelope，允许保留旧版本解密后重新加密。
- `.env.example` 的全零值只用于启动模板，不得用于共享环境；真实 Provider 凭据在生产必须通过 Vault/KMS Resolver 获取。
- 配置结构只提供 Key Material 和版本，业务 Secret 仍通过 `providersecret` 加密后入库；日志、错误与 Snapshot 均不得包含二者明文。

## 虚拟 Key 摘要配置

- `VIRTUAL_KEY_HASH_KEY`：32 字节 HMAC 根密钥的 64 位十六进制编码。测试、预发布和生产必须显式配置。
- `VIRTUAL_KEY_HASH_KEY_VERSION`：1～64 位非秘密版本标识，写入 `hash_key_version`，用于后续无停机摘要密钥轮换。
- 仅 `development` 可在显式 Key 为空时，从 32 字节 `LOCAL_ENVELOPE_KEY` 使用 HMAC-SHA-256 和版本化上下文标签派生域分离 Key；其他环境 fail closed。
- 配置加载、错误和日志都不能输出密钥值；调用方获得副本后在构造 Digester 后立即清零临时切片。
- `VIRTUAL_KEY_AUTH_CACHE_TTL`：数据面正缓存 TTL，默认 2 秒，允许 `0s`～`30s`；`0s` 禁用缓存。它是吊销/状态变更主动失效失败时的最大陈旧上界，不能无限放大。

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
