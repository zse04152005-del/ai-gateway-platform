# internal

内部模块必须遵守以下依赖方向：

```text
transport -> application -> domain
infrastructure -> application/domain ports
domain 不依赖 transport、数据库、Redis、Kafka 或供应商 SDK
```

计划模块：

- `identity`：租户、项目和虚拟 Key。
- `catalog`：Provider、LogicalModel、Deployment 和 Capability。
- `routing`：候选过滤、决策、健康、重试和熔断。
- `proxy`：普通与 SSE 代理、取消和背压。
- `limits`：RPM/TPM/并发。
- `budget`：Account、Reservation 和 Settlement。
- `metering`：Usage Event、Ledger 和价格。
- `config`：版本化 Snapshot。
- `security`：脱敏、密钥端口和 SSRF 防护。
- `observability`：日志、指标和 Trace。
- `gateway`：数据面 HTTP 健康接口与进程生命周期；不承载领域规则。
