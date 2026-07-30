# Request ID 与 W3C Trace Context

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P03-T07

## 1. 目标与边界

所有 gateway/control-plane HTTP 请求必须拥有服务端确认唯一的 Request ID，以及可跨 HTTP 进程传播的 W3C Trace Context。关联信息用于日志、Trace、错误响应和调用链定位，但绝不能作为身份认证、租户选择或幂等事实源。

## 2. Request ID 规则

- Header：`X-Request-Id`；允许字符为字母、数字、`.`、`_`、`:`、`-`，长度 8～128。
- 恰好一个合法客户端值、且当前服务的活跃/近期集合中不存在时才接受。
- 缺失、非法、多值、并发冲突或近期重放时，生成 `req_` 加 128-bit CSPRNG 十六进制值。
- 活跃请求单独跟踪，不受 TTL 影响；请求完成后进入默认 10 分钟近期窗口。
- 近期窗口默认最多 10,000 个 ID，满时淘汰最早过期项，避免攻击者造成无界内存增长。
- 最终 ID 在请求 Context、响应 Header 和统一错误响应 `request_id` 中保持一致。

这里的“冲突”同时覆盖并发使用和窗口内的顺序重放。跨不同服务允许同一 Request ID，因为它正是跨进程关联键；每个服务各自维护冲突集合。

## 3. Trace Context 规则

- 严格接受 W3C `traceparent` v00：`00-{32hex trace-id}-{16hex parent-id}-{2hex flags}`。
- Trace ID、Parent ID 不得全零；大写、额外字段、多 Header 或格式错误均视为无效父上下文。
- 有效父上下文保留 Trace ID、Parent Span ID 和 Flags；每个服务请求生成新的 64-bit Span ID。
- 无效/缺失父上下文生成新的 128-bit Trace ID，并使用部署配置的默认 Flags（当前 `00`）。
- `tracestate` 只有在父 `traceparent` 有效时才接收；限制 512 字节、32 个成员、唯一合法键和可见安全值。
- 响应 `traceparent` 包含当前服务 Span；下游 `InjectHTTP` 使用当前 Span 作为远端 Parent，从而构成父子链路。

## 4. 失败语义

若操作系统随机源失败或生成器连续返回冲突/非法 ID，中间件不进入业务 Handler，返回统一：

```json
{
  "error": {
    "code": "CORRELATION_CONTEXT_FAILED",
    "message": "Unable to initialize request correlation",
    "type": "gateway_error",
    "param": null,
    "request_id": "",
    "retryable": false,
    "retry_after_ms": null
  }
}
```

内部熵源错误作为私有 cause 保留，不进入响应。

## 5. 测试要求

- 合法/非法/多值 ID、并发冲突、近期重放、TTL 到期和容量淘汰。
- 合法/非法/全零 Trace ID/Span ID、重复 tracestate 键及边界长度。
- 两个独立 Manager 模拟两个进程，证明 Request ID/Trace ID 一致、下游 Parent 等于上游 Span。
- 熵源失败返回安全 ErrorEnvelope，响应不包含内部设备或路径信息。
- 并发测试必须在远程 Linux `go test -race` 下通过。
