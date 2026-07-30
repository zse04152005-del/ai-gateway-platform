# provideradapter

`provideradapter` 定义 Provider Adapter 的运行时端口和不可变 Factory 注册表；它依赖纯领域 `adapter` 规范化类型与 `catalog` Provider/Deployment，但不实现具体供应商协议。

核心边界：

- 仅允许在进程装配时通过 `NewRegistry(factories...)` 显式注册编译进二进制的 Factory；不加载运行时共享库、脚本或网络插件。
- Adapter Type 必须是小写规范标识；重复、nil/typed-nil 或非法 Factory 会使 Registry 构造失败。
- Registry 构造后没有 Register/Replace/Delete API，Type Map 与排序 Type List 不再变化，可被数据面并发无锁读取。
- `Resolve` 只做精确匹配，不使用默认 Adapter 或大小写回退；未知类型返回 `ErrUnknownAdapterType`。
- `ValidateStartup` 在进程 Ready 前校验完整 Provider 集合；`ValidatePublication` 在候选目录提交前校验完整集合。两者聚合非法 Provider、重复 ID/Code 和未知 Type，任何问题都会拒绝整个阶段。
- `Build` 先校验 Provider/Deployment 完整领域不变量和归属，再调用对应 Factory；传入 Deployment 是深拷贝，Factory 不能修改调用者的 Secret Reference 指针。
- Factory 返回 nil、声明身份发生变化、或 Adapter Type 不一致都会 fail closed；不会默默使用其他适配器。
- `FactoryError.Error()` 只输出 Type、Deployment ID 和稳定错误，不拼接 Factory cause；私有 cause 仍可用 `errors.Is/As` 诊断。
- Factory 自身必须并发安全，并通过构造注入 HTTP Client、Secret Resolver 等依赖；Registry 不持有凭据，也不把凭据写入错误。

P05-T04 将注册 Mock Factory 并使用真实本地 Mock Provider HTTP 协议实现全部 Adapter 方法；P05-T05 会把 Registry 构造/解析也纳入一致性套件。
