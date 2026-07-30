# P06-T04 上游 HTTP Client 验收报告

## 1. 结论

P06-T04 已在工作副本完成代码、专项测试和完整本地门禁。Gateway 进程启动时只创建一个并发安全的 Provider HTTP Client；连接、TLS、响应 Header、非流式总超时和连接池全部显式配置。出站请求不读取环境代理、不跟随重定向，并剥离客户端代理链、Cookie、Proxy Authorization、Host 覆盖与 hop-by-hop Header。

本报告不把尚未实现的普通响应归一化标记为完成；Handler 在 P06-T05 前仍不会返回伪造的模型成功响应。

## 2. 实现清单

- 新增 `internal/upstreamhttp.Client`，封装共享 `http.Client` 和 `http.Transport`。
- TCP connect/keepalive、TLS handshake、response header、total、idle 与 expect-continue timeout 独立配置。
- 全局 idle、单 Origin idle、单 Origin 总连接数及最大响应 Header 字节数均有启动期边界校验。
- TLS 最低 1.2、HTTP/2 协商开启、透明压缩关闭。
- `Proxy=nil`，不让进程环境变量隐式改变供应商流量路径。
- `CheckRedirect` 返回原始 3xx，禁止携带供应商认证 Header 跨 Origin。
- `Do` 克隆请求，不修改 Adapter-owned 原请求；清除自定义 Host、Trailer/Transfer-Encoding、Cookie、Proxy/Forwarded/Via/IP 和 Connection 扩展 Header。
- 保留 Adapter 最短凭据边界产生的 `Authorization`/供应商特性 Header；客户端 Virtual Key 不存在复制到上游的路径。
- 传输失败只返回 `ErrTransport`、`ErrTimeout`、Context 取消等稳定分类，不向公共层透传底层网络错误文本。
- `cmd/gateway` 在监听前创建唯一 Client，并在退出时释放 idle 连接。
- `.env.example`、配置说明、Gateway/内部模块说明和架构文档同步更新。

## 3. 自动化验证

### 3.1 专项测试

`internal/upstreamhttp` 使用真实 `httptest.Server`/TCP 连接验证：

- Transport 字段与配置逐项一致，TLS 最低版本为 1.2。
- 两次请求只建立一个新连接，证明连接池复用而非每请求新建 Client。
- 307 响应不被跟随，第二个 Origin 收不到请求。
- Provider Authorization/特性 Header 保留；Cookie、代理凭据、转发链、Connection 扩展字段被移除。
- 客户端自定义 Host 不进入 Provider，请求 Header 原值不被修改。
- response-header timeout 和 total timeout 均归类为 `ErrTimeout` + `ErrTransport`。
- 已取消 Context 保留 `context.Canceled`；nil、UserInfo/非法 Scheme 等请求 fail closed。
- 所有配置非法值和池大小交叉约束均被拒绝。

专项覆盖率：`93.8%` statements；其中 `NewClient`、`validate`、`sanitizeHeaders` 和 `CloseIdleConnections` 为 `100%`。

### 3.2 稳定性与静态门禁

- 相关包 `internal/upstreamhttp`、`internal/config`、`cmd/gateway`：`20/20` 轮稳定测试通过。
- `go vet`：通过。
- golangci-lint：`0 issues`。
- 完整门禁的普通与 integration build-tag lint：均为 `0 issues`。

### 3.3 完整仓库门禁

执行：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action check
```

结果：模块校验、格式、vet、双 lint、全包测试、全包构建、`govulncheck`、6 个迁移顺序、Actionlint 和本地高风险秘密扫描全部通过；`govulncheck` 输出 `No vulnerabilities found.`。

## 4. 安全审视

“不透传敏感 Header”由两层保证组成：第一层是 Gateway 只把业务字段转换为无 Header 的 `NormalizedRequest`，Adapter 创建全新上游请求；第二层是共享 Client 在发送前清理跨代理/浏览器信任域的 Header。不能简单删除所有 `Authorization`/`X-API-Key`，否则会破坏供应商认证；这些 Header 只能由部署绑定的 Adapter 在秘密解析最短边界生成。

底层网络错误可能带 URL 或本地连接信息，所以 Client 不把原始错误字符串包装进稳定错误。后续统一响应映射只依赖 `errors.Is` 分类。

## 5. 延后项

- P06-T05：Registry/Adapter 构建、HTTP 执行、普通响应解析与统一客户端输出。
- P06-T07：把客户端取消结果写入 Attempt 状态；底层 Context 传播已具备。
- P07：流式专用 deadline 与首 Token/no-progress timeout，不能复用非流式固定总超时语义。
- P08：重试、故障切换、健康和熔断。
- P12-T06：DNS/IP、重绑定、解析后地址及网络出口 SSRF 防护。
