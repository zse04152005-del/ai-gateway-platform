# Runtime Configuration

`config.Load` 只读取进程级环境配置。Provider、模型、价格、预算和路由属于版本化业务配置，不进入该结构。

本地 `.env` 由启动工具加载，应用本身不隐式搜索文件，避免生产环境意外读取工作目录中的秘密。

