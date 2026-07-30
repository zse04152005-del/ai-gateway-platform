# 架构文档

用于记录系统上下文、领域模型、数据流、SLO、技术栈和部署拓扑。架构图必须标记事实源、异步边界与信任边界。

已实现的横切设计：

- [`request-correlation.md`](request-correlation.md)：Request ID 冲突治理与 W3C Trace Context 传播。
