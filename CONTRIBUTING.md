# 贡献指南

先按 [开发指南](docs/development.md) 安装依赖与创建隔离配置。应用入口在 `cmd/paper-digest/`，领域代码在 `internal/`，前端在 `web/`；完整职责见 [运行与目录职责](docs/architecture.md)。保持包路径、配置、CLI 与已有数据兼容，目录搬迁需同步引用和构建配置。

提交前执行：

```bash
make check
make preview
git diff --check
```

Go 改动用 `gofmt` 格式化；行为变化补充能验证结果的测试。页面改动除构建检查外，使用独立演示数据库确认论文库、日报历史和主题隔离。不要用真实数据库或群机器人进行开发验证。不要提交 `config/config.json`、`config/config.local.json`、`.env`、数据库、备份或凭据。

PR 描述说明具体问题、改动后的行为和验证结果；配置、运行流程、API、路径变化应同步文档。处理 review 对话和失败状态后，再按仓库实际分支保护规则合并。是否允许合并以 GitHub 的实际检查和保护规则为准。
