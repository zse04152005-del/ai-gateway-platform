# 开发规范

- 以根目录《开发执行清单.md》为进度事实源。
- 每个任务通过验收后再标记完成。
- 公共行为变化必须更新测试和文档。
- 禁止在代码、Fixture、日志和提交历史中保存真实密钥或未脱敏内容。
- Go 依赖的用途、版本和许可证登记见 `dependencies.md`。
- 单元、Race、进程集成与 E2E 分层及模板见 [`testing-strategy.md`](testing-strategy.md)。
- Mock Provider 的场景 ID、错误契约与安全边界见 [`mock-provider.md`](mock-provider.md)。
