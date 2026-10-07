# 文档目录

[项目首页](../README.md) · [贡献指南](../CONTRIBUTING.md)

| 文档 | 职责 |
| --- | --- |
| [configuration.md](configuration.md) | JSON 字段、实际配置与示例、主题、模型网关、Webhook 和旧配置迁移 |
| [development.md](development.md) | 环境、构建、双终端开发、离线演示和 CLI |
| [operations.md](operations.md) | Compose 部署、更新、试发、调度状态、备份与故障定位 |
| [architecture.md](architecture.md) | 数据流、接口边界、目录职责及兼容限制 |
| [2026-10-07-delivery-investigation.md](2026-10-07-delivery-investigation.md) | 当日格式修复、停推与数据库排查的历史证据 |

当前使用说明按职责维护，每项流程只在对应文档详细描述，README 保留最短可执行入口与导航。历史记录保留日期与原路径；记录里的本机端口、临时镜像和工作区状态不能作为通用部署指南。新增排查记录用 `YYYY-MM-DD-主题.md` 命名并加入此索引。

命令统一假定从仓库根目录执行。配置或路径改变时，核对 README、本文档、Dockerfile、compose.yaml、Makefile 和离线演示脚本；不要把实际配置、Webhook、数据库或备份写入文档。
