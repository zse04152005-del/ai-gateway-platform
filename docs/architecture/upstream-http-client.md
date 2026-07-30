# 上游 HTTP Client 与连接池

## 1. 目标与边界

P06-T04 为所有 Provider HTTP 调用建立同一个进程级传输边界，解决三类常见生产问题：每请求创建 Client 导致端口/握手放大、默认超时缺失导致协程长期占用，以及把客户端 Header 或供应商凭据带到错误目的地。

本阶段只负责安全发送并返回原始 `http.Response`。Adapter 构建请求、普通响应归一化属于 P05/P06-T05；重试/熔断属于 P08；DNS/IP/重绑定 SSRF 判定属于 P12-T06。

## 2. 组件与生命周期

```text
Gateway bootstrap
    │ load + validate UPSTREAM_HTTP_*
    ▼
upstreamhttp.Client (one per process)
    │ owns
    ├── ordinary http.Client (fixed whole-response deadline)
    ├── streaming http.Client (Context-owned lifecycle, no fixed Body deadline)
    └── shared http.Transport (dial/TLS/header deadlines + shared pool)
            │
            ▼
      Adapter-owned http.Request ──► Provider
```

`cmd/gateway` 在监听前构造一次 Client，任何配置不合法都 fail closed；请求 Handler 后续只注入该共享实例，不允许在热路径调用 `http.Client{}`。进程退出时调用 `CloseIdleConnections`，在途请求仍由请求 Context 和 HTTP Server 排空语义管理。

## 3. 超时与资源上限

| 配置 | 默认值 | 作用 | 校验上限 |
|---|---:|---|---:|
| `UPSTREAM_HTTP_CONNECT_TIMEOUT` | 5s | TCP 连接 | 30s |
| `UPSTREAM_HTTP_KEEPALIVE` | 30s | TCP keepalive | 5m |
| `UPSTREAM_HTTP_TLS_HANDSHAKE_TIMEOUT` | 5s | TLS 握手 | 30s |
| `UPSTREAM_HTTP_RESPONSE_HEADER_TIMEOUT` | 60s | 等待 Provider 首个响应 Header | 10m |
| `UPSTREAM_HTTP_TOTAL_TIMEOUT` | 2m | 非流式请求从发送到读完 Body 的总时长 | 30m |
| `UPSTREAM_HTTP_IDLE_CONN_TIMEOUT` | 90s | idle 连接回收 | 10m |
| `UPSTREAM_HTTP_EXPECT_CONTINUE_TIMEOUT` | 1s | `100-continue` 等待 | 30s |
| `UPSTREAM_HTTP_MAX_IDLE_CONNS` | 512 | 全局 idle 上限 | 100000 |
| `UPSTREAM_HTTP_MAX_IDLE_CONNS_PER_HOST` | 64 | 单 Origin idle 上限 | 不高于全局 idle |
| `UPSTREAM_HTTP_MAX_CONNS_PER_HOST` | 128 | 单 Origin 总连接上限 | 不低于单 Origin idle |
| `UPSTREAM_HTTP_MAX_RESPONSE_HEADER_BYTES` | 65536 | 响应 Header 大小 | 1 KiB～1 MiB |

总超时适用于 P06 非流式执行。P07 流式连接通过 `DoStream` 绕开固定 2 分钟 Body deadline，同时仍受同一个 Transport 的连接、TLS、响应头、Header 大小和连接池约束。调用方在发送前创建 `streaming.TimeoutController`，把其 Context 贯穿 Adapter 构建和上游请求；独立 total timer 覆盖完整 Attempt，收到 Header 并 Attach 后再启动首模型 Token timer，首个内容/推理/工具 Delta 后切换为可重置的 no-progress timer。

## 4. TLS、代理与重定向

- TLS 最低版本为 1.2，并启用 HTTP/2 协商。
- 不读取 `HTTP_PROXY`/`HTTPS_PROXY` 等环境代理，避免 Provider 凭据被未经版本化配置的代理观察。
- 不自动跟随任何 3xx；上层收到原始 3xx 后按 Provider 错误处理，`Authorization`、`X-API-Key` 等 Header 不会跨 Origin 重放。
- 禁止 URL UserInfo 与非 HTTP(S) Scheme。更细的 Endpoint DNS/IP 安全策略在 P12-T06 加入。
- 禁用透明响应压缩，使 Adapter 的响应体大小限制面对真实传输字节，不因自动解压绕过边界。

## 5. Header 信任边界

客户端 Virtual Key 只用于 `keyauth` 生成可信 Principal，不进入 `NormalizedRequest`。Provider Adapter 使用规范化业务字段创建全新的 `http.Request`，并在最短边界解析供应商凭据。因此上游请求不存在“从入站请求复制全部 Header”的入口。

`upstreamhttp.Client.Do` 与 `DoStream` 仍做同一层第二层防御：先深复制 Header，再清空自定义 Host、Trailer 和 Transfer-Encoding，删除 `Connection` 声明字段以及 Cookie、Proxy Authorization、Forwarded/X-Forwarded、客户端 IP、Via、Upgrade 等跨边界元数据。原始 Adapter Request 不被修改。

供应商 `Authorization`、`X-API-Key` 和协议特性 Header 属于 Adapter-owned Header，不能一刀切删除；防止入站凭据混入的保证来自“全新请求 + 无 Header 字段的 NormalizedRequest”，而不是猜测 Header 名称。

## 6. 错误与取消

公共错误只暴露稳定分类：

- `ErrInvalidRequest`：Adapter 请求缺少 URL、Scheme 不受支持或包含 UserInfo。
- `ErrTransport`：没有得到可用响应的传输失败。
- `ErrTimeout`：连接、TLS、响应头或总 deadline 超时，同时满足 `errors.Is(err, ErrTransport)`。
- 调用 Context 取消时保留 `context.Canceled`/`context.DeadlineExceeded`，同时归入 `ErrTransport`。

原始网络错误字符串不会向上传递，避免把 Provider Endpoint、代理或底层连接细节带入公共响应。P06-T05 将这些稳定分类映射为统一 Gateway 错误。

## 7. 验证矩阵

- 配置默认值、解析错误、所有上限与连接池交叉约束。
- TLS 最低版本、HTTP/2、禁用代理/压缩、响应 Header 上限。
- 两次顺序请求只建立一个 TCP 连接，证明进程级复用。
- 3xx 不跟随，目标 Origin 没有收到请求。
- Adapter 认证/特性 Header 保留；Cookie、代理、转发链和 Connection 扩展 Header 被剥离；原请求不突变。
- 响应头超时、总超时、调用方取消、非法 URL 和 nil 请求的稳定错误分类。
- `DoStream` 在收到 Header 后不会被普通请求的固定总超时截断，生命周期改由调用 Context 管理。
