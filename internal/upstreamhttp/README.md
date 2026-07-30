# Upstream HTTP Client

本包拥有 Gateway 进程级、并发安全的 Provider HTTP Client 与连接池。调用方必须在进程启动时只创建一次，并在优雅关闭时调用 `CloseIdleConnections`；业务请求不能临时创建 `http.Client` 或 `http.Transport`。

安全边界：

- Provider Adapter 构造一份全新的上游请求；客户端入站 Header 不进入 `NormalizedRequest`，因此不存在整包复制。
- `Do` 发送前克隆请求并移除 Cookie、Proxy Authorization、Forwarded/X-Forwarded、客户端 IP、Connection 声明字段及其他 hop-by-hop Header，不修改 Adapter 原请求。
- Adapter 自己生成的 Provider `Authorization`/`X-API-Key` 等认证 Header 必须保留；本包不会把二者误判为客户端凭据。
- 禁止 URL UserInfo、非 HTTP(S) Scheme、环境代理和自动重定向，防止凭据被环境配置或跨 Origin 3xx 带走。
- Transport 最低 TLS 1.2、启用 HTTP/2、禁用透明压缩，并限制响应头大小。

超时分层：连接、TLS 握手、响应头、总请求、空闲连接和 Expect-Continue 独立配置。P07 流式请求接入时将使用无全程 deadline 的流式专用执行路径，但仍复用相同的安全 Transport；P12-T06 在 Adapter Endpoint 校验之外增加 DNS/IP/重绑定和重定向目的地 SSRF 策略。
