# mock-provider

本地 Provider 协议模拟进程，默认监听 `127.0.0.1:18082`。

```powershell
powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1 -Action mock-provider
```

它复用共享健康、关联、连接跟踪和优雅关闭骨架，但使用独立的最小配置加载器，不连接数据库或其他基础服务。`APP_ENV=staging|production` 或非 Loopback 监听地址会在打开 Listener 前失败。

场景与请求示例见 [`docs/development/mock-provider.md`](../../docs/development/mock-provider.md)。
