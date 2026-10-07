# 运行与目录职责

[文档导航](README.md) · [配置](configuration.md) · [部署与运维](operations.md)

## 数据流与边界

`cmd/paper-digest` 加载 JSON、打开 SQLite、迁移旧 Webhook 并启动 HTTP 服务；`delivery.enabled=true` 时才校验模型密钥、恢复中断发送状态、启动单个 worker。HTTP 服务与 worker 共用 Store，取消与退出时等请求和任务完成后关闭数据库。

worker 在北京时间 08:00～09:00 获取 arXiv 最新公开元数据（最多 100 条），按首次发布日期、主题关键词和已确认的推荐历史筛选，按发布日期降序最多入选 5 篇。联合主题覆盖推荐、广告或搜索任一方向，搜索还接受 `cs.IR` 分类。不会抓取全文、项目页或解读网页，新增主题 ID 不会自动产生新的抓取或调度规则。

候选论文、版本和逐篇中文摘要写入 SQLite，全部完成后将日报记为 `ready`。模型摘要保存模型名称和提示版本；新摘要使用 `arxiv-summary-v2`，要点格式为 `- **标签**：正文`。渲染日报和发送卡片时也会规范历史标签，不改写存档。摘要完整性校验检查数学分隔符并跳过 Markdown 代码和链接目标。

09:00 的一分钟内发送完整日报，整批先预检卡片大小、再写入发送意图。每篇确认后立即保存去重历史，整批完成才记为 `sent`；中途失败停止后续发送并记为 `unknown`。未就绪或错过窗口记为 `missed`，不自动补发。详细窗口、重启和状态语义见 [运维指南](operations.md#调度与状态)。

论文库保存每日入选项，**不是全部抓取结果**。按最近入选日期排列并合并稳定 ID；按日期查看时保留当日版本与摘要。未完成中文摘要也可浏览。主题之间按 ID 隔离论文、日报与 Webhook。页面把选中主题放入 URL，支持刷新、前进后退和分享。

## 目录职责

```text
paper-digest/
├── cmd/paper-digest/       CLI、依赖装配、HTTP/worker 生命周期
├── internal/
│   ├── config/            JSON 加载、主题和 URL 校验
│   ├── papers/            arXiv Atom 抓取、稳定 ID、选题和排序
│   ├── digest/            模型分析、格式与数学校验、日报/卡片正文渲染
│   ├── state/             SQLite 结构、状态转换、查询、备份与 Webhook 存储
│   ├── job/               北京时间调度、生成、逐篇投递与恢复
│   ├── delivery/          飞书请求体、大小预检和响应判定
│   └── web/               HTTP API、设置校验、健康检查和静态托管
├── web/                   React + TypeScript + Vite 前端
│   ├── src/               页面、hooks、类型和样式
│   └── public/licenses/   前端字体许可证
├── config/                可提交示例；被忽略的实际配置
├── docs/                  配置、开发、运维、架构与历史记录
├── testdata/              离线 Markdown fixtures、虚构界面数据生成脚本
├── Makefile               常用开发命令（默认 help）
├── Dockerfile             多阶段镜像构建
└── compose.yaml           单副本本机部署与持久化挂载
```

测试与实现同目录，保持 Go 常规布局。生成的 `web/dist/`、`web/node_modules/`、`bin/`、`data/` 和 `backups/` 均被忽略；不将构建产物或运行数据纳入源码。数据库 schema 及升级逻辑位于 `internal/state/sqlite.go`，没有独立迁移命令，打开数据库时自动初始化。查看状态和备份也会打开数据库；仅在启用 worker 的 `serve` 中恢复发送状态。

## HTTP API

前端使用 Go 同源 API；开发时 Vite 代理 `/api`。下面列出当前路由，字段实现见 [server.go](../internal/web/server.go) 和 [前端类型](../web/src/types.ts)。

| 方法与路径 | 用途 |
| --- | --- |
| `GET /api/health` | HTTP + SQLite 查询健康，成功返回 `{"status":"ok"}` |
| `GET /api/topics` | 主题 ID、显示名称与自动任务启用状态；不暴露 Webhook |
| `GET /api/papers` | 支持 `q`、`date`、`summary`、`page`、`pageSize` 的论文列表 |
| `GET /api/papers/detail?id=...` | 稳定 ID 对应详情；可用 `date` 定位当日版本 |
| `GET /api/digests` | 分页日报历史 |
| `GET /api/digests/YYYY-MM-DD` | 当天完整日报与论文 |
| `GET /api/settings/webhook` | 仅返回是否已配置及自动任务启用状态 |
| `PUT /api/settings/webhook` | JSON `{"webhookURL":"https://..."}` 保存地址，空字符串清除；不会试发 |

论文、日报和设置接口均接受 `?topic=<id>`；省略时优先联合主题，否则取配置首个主题，未知主题返回 404。主题名称没有独立 JSON 字段：联合主题显示「推荐 / 广告 / 搜索」，其他主题显示 ID。分页参数和筛选值的校验见查询实现。

设置写入要求 localhost 或回环 IP 的 Host，Origin 若存在须与 Host 同源；接口不回显地址。无登录功能，反向代理对外开放阅读时需配置访问控制，并保留 Host。数据库及备份包含 Webhook 密钥，必须私密保管。
