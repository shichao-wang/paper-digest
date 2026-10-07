# 论文日报

Go + React 的本机论文日报服务。每天北京时间 08:00～09:00 从 arXiv 公开元数据中筛选推荐、广告或搜索相关论文，最多 5 篇，根据公开摘要生成中文要点；09:00 的一分钟内向飞书群机器人逐篇发送 Markdown 卡片。SQLite 保存每日精选、摘要版本、发送状态与去重历史，网页提供论文库、日报历史和主题推送配置。

**默认关闭自动任务**：示例中的 `delivery.enabled=false` 同时关闭采集、模型调用和推送，仍可浏览已有数据。空库不会自动填入示例。中文要点依据公开摘要，不能视为全文解读；当前自动任务只支持 `recommendation-advertising-search` 联合主题。

## 快速上手：Docker Compose

需要 Docker Engine + Compose 插件，或 Docker Desktop。以下命令均在仓库根目录执行：

```bash
cp config/config.example.json config/config.json
chmod 600 config/config.json
# 首次保持 delivery.enabled=false；真实凭据只填实际配置
docker compose config --quiet
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 paper-digest
```

打开 http://127.0.0.1:8080。首次页面为空是正常行为；要离线体验界面，使用 [虚构数据演示](docs/development.md#离线界面演示)。

实际配置只读挂载到容器，数据库保存在 `digest-data` 命名卷中。容器重建保留数据；`docker compose down -v` 会删除数据卷。服务只监听宿主机回环端口，没有登录功能。不要扩为多个定时 worker。

启用自动任务前，按 [配置说明](docs/configuration.md) 填写模型服务信息，在页面「主题管理」保存 Webhook，并按 [受控试发流程](docs/operations.md#受控飞书试发) 核对目标群与消息。再将 `delivery.enabled` 改为 `true`、重新创建容器；模型调用会产生费用，机器与 Docker 必须在调度时段在线。

## 本地开发与验证

需要 Go 1.27、Node 24；离线界面演示另需 Python 3：

```bash
npm --prefix web ci
make check   # Go 测试、vet、TypeScript 检查与前端构建
make build   # 前端与 Go 二进制，输出 bin/paper-digest
make preview # 仅渲染离线 fixture，不访问外部服务
```

完整的宿主机配置、前后端双终端开发与演示步骤见 [开发指南](docs/development.md)。这些命令不会发送群消息或调用模型。

## 文档导航

| 目的 | 文档 |
| --- | --- |
| 配置 JSON、模型网关、主题与 Webhook | [配置说明](docs/configuration.md) |
| 本地开发、离线演示、构建与验证 | [开发指南](docs/development.md) |
| 部署、试发、调度状态、备份与故障定位 | [部署与运维](docs/operations.md) |
| 数据流、API 和目录职责 | [运行与目录职责](docs/architecture.md) |
| 提交改动与 review | [贡献指南](CONTRIBUTING.md) |
| 历史排查与文档维护约定 | [文档目录](docs/README.md) |

仓库已有真实推送与本机排查记录；它们是特定日期的历史证据，不代表新部署完成验收。见 [2026-10-07 排查记录](docs/2026-10-07-delivery-investigation.md)。
