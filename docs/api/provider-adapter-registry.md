# Provider Adapter 注册表契约

> 状态：Implemented
>
> 日期：2026-07-30
>
> 对应任务：P05-T03
>
> 实现：`internal/provideradapter`

## 1. 目标

企业网关不能在发现未知 `adapter_type` 时临时猜测协议，也不能等到首个生产请求才暴露配置错误。注册表把“代码中支持哪些协议”和“Catalog 发布了哪些 Provider”做成一个可执行门禁：

- 按 Type 精确加载 Factory；
- 未知 Type 在启动或发布阶段失败；
- 重复、nil、类型错配与 Factory 异常 fail closed；
- 注册完成后并发读取无锁、结果确定。

## 2. 为什么不用运行时动态插件

当前 MVP 只允许显式编译进二进制的 Factory：

- 供应链和依赖版本可由 `go.mod`、SBOM/漏洞扫描与 CI 统一审计；
- 不需要在数据面开放任意文件、脚本或动态库加载权限；
- Adapter 类型集合在一个进程生命周期内不可变化，不会出现同一 Type 被热替换后语义漂移；
- 新增 Adapter 仍可通过小型 Factory 注册和统一 Conformance Suite 扩展。

未来若确需进程外插件，应使用版本化 RPC 与独立沙箱，而不是把未审计代码加载到 Gateway 进程。

## 3. 运行时接口

```go
type Factory interface {
    Type() Type
    New(
        ctx context.Context,
        provider catalog.Provider,
        deployment catalog.Deployment,
    ) (Adapter, error)
}

type Adapter interface {
    Type() Type
    Capabilities(ctx context.Context) catalog.CapabilitySet
    BuildRequest(ctx context.Context, in adapter.NormalizedRequest) (*http.Request, error)
    ParseResponse(ctx context.Context, resp *http.Response) (adapter.NormalizedResponse, error)
    OpenStream(ctx context.Context, resp *http.Response) (ChunkStream, error)
    NormalizeError(ctx context.Context, resp *http.Response, body []byte) adapter.NormalizedError
    EstimateUsage(ctx context.Context, in adapter.NormalizedRequest) (adapter.NormalizedUsage, error)
}
```

Factory 是依赖注入边界：HTTP Client、Secret Resolver、Clock、Tokenizer 或协议版本由具体 Factory 持有。Registry 只保存 Factory，不保存 Provider Key、已解析 Secret 或客户端请求内容。

Adapter 实例绑定一个已验证 Provider/Deployment。`Build` 先执行：

1. Context 非 nil 且未取消；
2. Provider 与 Deployment 各自领域校验通过；
3. Deployment 的 `provider_id` 等于 Provider ID；
4. Provider `adapter_type` 精确命中 Factory；
5. Factory 当前声明 Type 仍与注册 Type 一致；
6. 返回的 Adapter 非 nil 且 Type 一致。

任一步失败都不调用或不返回可用 Adapter，不存在默认回退。

## 4. Registry 构造

```go
registry, err := provideradapter.NewRegistry(
    mockFactory,
    anotherFactory,
)
```

构造会拒绝：

- nil 或 typed-nil Factory；
- 不符合 `^[a-z][a-z0-9._-]{0,63}$` 的 Type；
- 重复 Type。

Registry 没有公开的 Register、Replace 或 Delete。`Types()` 始终返回排序防御性副本，调用者不能修改内部集合。`Resolve()` 是大小写敏感的精确查找。

## 5. 启动门禁

进程完成 Catalog Snapshot 加载、但标记 Ready 之前调用：

```go
if err := registry.ValidateStartup(snapshot.Providers); err != nil {
    return fmt.Errorf("validate provider adapters: %w", err)
}
```

未知 Type 返回可被 `errors.Is(err, ErrUnknownAdapterType)` 识别的聚合错误。启动层必须保持 Not Ready 并退出或等待一份已知有效 Snapshot，不能跳过 Provider。

## 6. 发布门禁

控制面在持久化/广播候选 Catalog 之前，对“完整候选 Provider 集合”调用：

```go
if err := registry.ValidatePublication(candidate.Providers); err != nil {
    // reject the entire candidate; current published version remains active
}
```

校验一次聚合并确定性排序以下问题：

- Provider 本身领域不合法；
- 重复 Provider ID；
- 大小写归一后的重复 Provider Code；
- 未注册 Adapter Type。

错误对象通过 `Problems()` 返回防御性副本，适合控制面生成逐项诊断；`Error()` 只给阶段和问题数量。任何 Problem 都拒绝整个候选版本，避免部分发布。

## 7. Factory 失败与信息安全

Factory 可能因为客户端装配、协议版本或依赖不可用失败。`FactoryError`：

- 对外 Error String 只包含 Adapter Type、Deployment ID 和 `ErrFactoryFailed`；
- 不拼接底层 `cause.Error()`，避免某个 Secret Resolver 把凭据写入普通日志；
- 通过 `Unwrap` 保留 `errors.Is/As`，受控诊断仍能识别内部 cause；
- nil Adapter、Factory Type 漂移和返回 Adapter Type 错配均视为 Factory 失败。

调用方仍必须把内部错误映射为统一安全 API Error，而不是直接返回给客户端。

## 8. 并发与生命周期

- Registry 只在构造时写 Map，之后没有写路径，`Resolve/Has/Types/Build` 可并发调用。
- Factory 必须并发安全；真实 Adapter 应使用可复用、受限连接池的 HTTP Client，而不是每请求创建 Transport。
- Registry 不负责热替换 Factory。Catalog 可热发布 Provider/Deployment，但 `adapter_type` 必须属于当前二进制已注册集合。
- 新二进制支持新 Type 后，先部署/Ready，再发布引用该 Type 的 Catalog；回滚时反向执行，避免旧二进制读到未知 Type。

## 9. 自动化验收

单元测试覆盖：

- 排序、防御性副本、精确 Resolve 与未知 Type；
- nil、typed-nil、非法和重复 Factory；
- startup/publication 对已知、未知、非法与重复 Provider 的聚合拒绝；
- Provider/Deployment 校验、跨 Provider Deployment 和已取消 Context 在 Factory 前失败；
- Deployment Secret Reference 指针防御性复制；
- Factory 私有错误不进入 Error String，但 `errors.Is` 仍可识别；
- nil Adapter、Factory Type 变化、返回 Adapter Type 错配 fail closed；
- 64 个并发 Resolve + Build，在 Linux CI race detector 下验证不可变 Registry 读路径。

P05-T04 已以 `mock` Factory 执行真实注册与 HTTP 转换；P05-T05 已用同一 Factory/Registry 路径接入统一 [`Adapter Conformance Suite`](./adapter-conformance-suite.md)，没有在测试中直接构造私有 Adapter。P11 配置发布实现必须调用本文件的 publication 门禁，不能复制一份宽松规则。
