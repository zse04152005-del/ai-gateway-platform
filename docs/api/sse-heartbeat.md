# Gateway SSE heartbeat

## 1. Wire contract

Gateway heartbeat 是 SSE comment，不是模型事件：

```text
: gateway-heartbeat

```

它不包含时间戳、Request ID、模型名、Provider、租户或任何用户内容。客户端应按 SSE 规范忽略 comment；只解析 `data:` 的 OpenAI-compatible 客户端不会观察到模型语义变化。

## 2. Client preference

请求可携带：

```http
X-AI-Gateway-SSE-Heartbeat: off
```

规则：

- Header 缺失或精确值 `on`：使用平台配置的 interval；
- 精确值 `off`：本请求不创建 heartbeat ticker；
- 其他值（包括自定义 interval、大小写变体、空白前后缀和多值歧义）：fail closed；
- 客户端只能关闭或恢复平台策略，不能选择发送频率，避免恶意/错误配置造成小包与 Flush 放大。

## 3. Runtime semantics

`streaming.Heartbeat.Run` 由流式执行器显式拥有生命周期：

1. 每个启用的请求最多创建一个 ticker；禁用请求不分配 ticker。
2. 到期时调用并发安全的 `sse.Writer.WriteComment`，沿用逐事件 write deadline 和立即 Flush。
3. 只有完整 comment 成功写入并 Flush 后，才调用 `TimeoutController.RecordGatewayHeartbeat` 并增加统计。
4. Context 取消、客户端断开、写超时或 recorder 状态冲突会停止 loop；ticker 总会释放。
5. Runner 自己不创建 Goroutine，调用方必须显式启动、等待和回收，因此不会产生不可追踪的后台任务。

## 4. Semantic isolation

Gateway heartbeat：

- 不创建 `adapter.NormalizedChunk`；
- 不占用 Provider Chunk sequence；
- 不写 `data:`，不改变 Content/Reasoning/Tool Delta；
- 不满足首 Token deadline；
- 不重置上游 no-progress deadline；
- 不进入 Usage、计费或 Prompt/Response 审计；
- 只在无内容 Snapshot 中记录成功次数与最近发送时间。

Provider 自己的 heartbeat 仍是一个有界上游 Chunk：首 Token 前不能冒充模型输出，首 Token 后可证明 Provider 链路仍有真实活动。两类 heartbeat 因此不会混淆。

## 5. Compatibility

SSE comment 属于标准 EventSource framing。某些反向代理有自己的 idle timeout，平台 interval 应低于已知链路的最小 idle timeout，同时不能低到制造明显 Flush/网络成本。组件边界允许 10 ms～5 min，生产配置应采用秒级值；客户端 opt-out 可用于对 comment framing 不兼容的旧 SDK。
