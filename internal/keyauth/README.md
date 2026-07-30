# keyauth

数据面 Virtual Key 认证模块：

- 严格解析一个 Bearer Header 和 `safe-prefix.base64url-secret`，拒绝多 Header、额外点号、非规范 base64url 和非 32 字节 Secret。
- 用安全前缀查询 PostgreSQL，查询同时 JOIN Tenant/Project 状态。
- 根据 `hash_key_version` 从 Keyring 选择 HMAC-SHA-256 Digester，并用 `hmac.Equal` 比较固定 32 字节摘要。
- 向 Context 写入深拷贝 `Principal`；下游只能从 Context 获取 Tenant/Project/VirtualKey，不能信任请求 Header。
- Principal 深拷贝保留模型策略三态，确保显式空白名单不会被误解释为继承。
- 缺失、错误、吊销、过期和禁用统一为 `INVALID_API_KEY`；数据库/未知摘要版本为 `AUTHENTICATION_UNAVAILABLE`。
- `MemoryCache` 只缓存正向 Record，容量和 TTL 有界，返回深拷贝并支持 `Invalidate(prefix)`；完整凭据不进入缓存。

缓存命中后仍重新检查 `expires_at` 与 `rotation_grace_expires_at`，时间边界不受 TTL 影响。吊销、Tenant/Project 状态变更依赖显式失效或 TTL 收敛；跨进程主动通知接入前可将 TTL 配置为 `0s` 获得最严格一致性。
