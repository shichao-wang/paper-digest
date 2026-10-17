# Research Vault 命名与部署兼容

[文档导航](README.md) · [部署与运维](operations.md)

仓库从 `paper-digest` 更名为 `research-vault`。项目品牌与代码模块使用新名称，CLI、源码入口和二进制一起更名，部署标识继续保留以复用已有数据卷。

## 名称对应

| 用途 | 当前名称 | 处理方式 |
| --- | --- | --- |
| 产品品牌 | Research Vault | README、浏览器标题和侧栏统一使用 |
| GitHub 仓库 | `shichao-wang/research-vault` | remote 与仓库目录使用新名称 |
| Go 模块 | `github.com/shichao-wang/research-vault` | go.mod 与所有内部 import 一起更新 |
| 前端包 | `research-vault-web` | package.json 与 lock 文件一起更新 |
| CLI 与源码入口 | `research-vault`、`cmd/research-vault` | 一起更名，同步命令文档和本地启动配置 |
| 二进制 | `bin/research-vault`、`/usr/local/bin/research-vault` | 一起更名，Makefile、Dockerfile、入口与健康检查一致 |
| Compose 服务与默认项目 | `paper-digest` | 保留，compose.yaml 固定默认项目名，目录更名不再改变默认卷前缀 |
| Compose 卷键与默认实际卷 | `digest-data`、`paper-digest_digest-data` | 保留，实际部署仍须核对项目名、挂载与卷 |
| 功能名称 | 论文库、论文日报等 | 保留，功能与项目品牌分别命名 |
| 历史排查记录 | 当时使用的名称和路径 | 保留，作为历史证据 |

`-p` 和 `COMPOSE_PROJECT_NAME` 可以覆盖 compose.yaml 中的默认项目名。已有部署若使用自定义项目名，应继续使用该项目名。当前仓库路径不能证明实际部署使用哪个卷，切换前按运维指南核对。

## 调用方与验证

CLI、源码入口和二进制从 `paper-digest` 改为 `research-vault`。Makefile、Dockerfile、Compose 健康检查、`.claude/launch.json` 和 CLI 帮助同步新名称。旧命令不提供别名，外部脚本需要将 `paper-digest` 命令及路径改为 `research-vault`。Compose 服务名仍为 `paper-digest`，因此文档使用 `docker compose exec -T paper-digest research-vault ...`，避免依赖自动生成的容器名。

模块路径变更以 `go test ./...`、`go vet ./...` 验证。前端包和品牌以依赖安装、构建及隔离演示页面验证。`make check`、`make build`、`make preview` 和 `git diff --check` 用于最终核对。界面验证使用虚构数据并关闭自动任务，不使用真实数据库或群机器人。

部署核对、备份、切换与回退流程见[部署与运维](operations.md#仓库改名后的部署切换)。
