# 论文日报

本机 Docker Compose 运行的 Go 服务：每天北京时间 08:00 搜集 Recommendation / Advertising / Search **联合主题**的 arXiv 新论文，根据公开摘要生成中文要点；09:00 仅在日报完整就绪时，通过飞书群机器人 Webhook 每篇论文发送一张 Markdown 卡片。第一版不抓取全文、项目页或解读网页；历史论文、摘要版本、任务与发送状态保存在 SQLite 中。浏览器中的「论文库」提供已有每日精选的检索、摘要阅读和日报历史。

> 默认 **不调用模型、不发送飞书**。须自行确认目标群、机器人身份、内容及模型费用后，在未跟踪的 `config/config.json` 中设置 `delivery.enabled=true` 才启用实际任务。现阶段没有做过真实发送或 09:00 投递验收。

## 本机构建

```bash
cp config/config.example.json config/config.json
chmod 600 config/config.json
# 只在 config/config.json 中填真实凭据；首次保持 delivery.enabled=false
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 paper-digest
```

本机页面映射至 **http://127.0.0.1:8080**，仅监听宿主机回环地址；Cloudflare Tunnel 不是日报任务的依赖。`delivery.enabled=false` 时页面仍可浏览已有数据，采集、模型和飞书任务保持关闭。Docker Desktop、宿主机和网络必须在任务时段持续在线、未睡眠。Compose 仅运行一个副本，切勿扩为多个定时 worker。代码更新后在非 08:00～09:00 时段运行 `docker compose up -d --build`；容器重建不会删除 `digest-data` 数据卷。不要运行 `docker compose down -v`，那会删除数据。

```bash
go test ./...
go vet ./...
docker compose config
# 仅渲染离线 fixture，不读数据库、不使用密钥、不访问网络
go run ./cmd/paper-digest preview ./testdata/preview.json
# 查看当天任务状态
docker compose exec paper-digest paper-digest status
```

## 论文库与前端开发

启动 Compose 后打开 http://127.0.0.1:8080。左侧导航展示运行配置 `topics` 中登记的主题，点击主题后在内容区的「论文库／日报历史」之间切换；手机与窄屏通过顶部「主题导航」展开入口。主题保存在 URL 中，刷新、前进后退或分享链接时保留。论文库支持关键词搜索、日报日期和中文要点状态筛选；选择论文可阅读中文要点和原始英文摘要，或打开 arXiv / PDF。日报历史保留当天论文、正文和生成/发送状态。「主题管理」集中展示所有主题的 Webhook 与自动任务状态，逐条进入「编辑推送配置」保存、替换或清除地址；返回管理页时重新读取配置状态。

数据库保存的是每天入选日报的最多 5 篇，**不是全部抓取结果**。论文库按最近入选日报日期排列并合并重复 ID；按日期筛选或从日报进入详情时保留当天的版本和摘要。未完成中文要点的论文仍可浏览。中文要点依据公开摘要生成，不能视为全文解读。空库不会自动填充示例或抓取历史论文。

前端位于 `web/`，使用 React、TypeScript 和 Vite；Docker 自动构建静态资源，由 Go 同源托管。运行时不需要 Node。直接在宿主机开发需要 Node 24 和 Go 1.27：

```bash
npm --prefix web ci
```

```bash
npm --prefix web run build
```

准备独立的开发配置，保持 `delivery.enabled=false`，将 `database.path` 指向本地可写目录内的 SQLite 文件。不要将容器中的 `/data/digest.db` 路径直接用于宿主机：

```bash
go run ./cmd/paper-digest --config config/config.json serve
```

另一个终端启动前端，访问 http://127.0.0.1:5173；Vite 将 `/api` 代理至本机 Go 服务：

```bash
npm --prefix web run dev
```

离线查看界面可用 Python 3 创建独立的虚构数据（不会访问 arXiv、模型或飞书，也不会覆盖已存在的演示数据库）：

```bash
python3 testdata/seed-web-preview.py
```

```bash
go run ./cmd/paper-digest --config data/web-preview/config.json serve
```

脚本只写入被 Git 忽略的 `data/web-preview/`，创建两个主题，使用相同日期和论文 ID 的不同虚构内容验证隔离，所有标题均标记 `[Demo]`。实际论文库不加载这些数据。已有演示库时可用 `--output data/另一个目录` 创建独立数据，并通过其中的 `config.json` 启动服务。

`serve` 默认监听 `127.0.0.1:8080`，从 `web/dist` 读取资源，可使用 `--listen` 和 `--web-dir` 调整。Go 启动前须先构建静态资源。`health` 无需配置，通过本机 `/api/health` 检查 HTTP 和数据库；服务未运行时返回失败，可使用 `health --url <地址>` 检查其他监听地址。数据库打开和只读查询不会修改发送中的状态；中断发送恢复只在启用日报 worker 时执行。

## 主题与机器人配置

`config/config.example.json` 是可提交的示例，`config/config.json` 是 **Git 和 Docker 构建都忽略的实际配置**；后者通过 Compose 只读挂载到容器，不在镜像中。请在启动 Compose **之前**创建该文件，且仅在实际配置中填写模型密钥。当前 `recommendation-advertising-search` 是 Recommendation／Advertising／Search 联合主题。

飞书群机器人 Webhook 在「主题管理」中配置，点击目标主题所在行的「编辑推送配置」。每个主题独立保存地址；保存、替换或清除一个主题不会修改其他主题。粘贴完整 HTTPS 地址并保存后，地址保存在 SQLite 数据卷中，后续推送每次读取当前配置，无需重启容器。已保存的地址不回显；替换时填写新地址即可。「清除配置」经过确认后暂停当前主题推送，重新配置后恢复。保存、替换和清除均不会发送群消息，也不会补发历史日报。页面没有登录功能，设置写入仅接受 `localhost` 或回环 IP 的 Host，请从本机地址打开设置。代理需保留原始 Host；域名入口不能用于修改 Webhook，对外开放阅读页面时仍需配置访问控制。

已有 JSON 中的 `topics[].webhook_url` 仍兼容：`serve` 和已确认的 `send-test` 首次打开数据库时，将合法的旧地址迁入数据库，已有页面配置不会被覆盖，已清除的地址也不会复活。迁移后可从实际 JSON 中移除 `webhook_url`。数据库与备份包含 Webhook 密钥，应按凭据保管。服务打开数据库及保存 Webhook 前会将数据库、已有 WAL/SHM 收紧为 `0600`，新数据库与备份也以 `0600` 创建；无法设置权限时会报错停止操作。

配置文件还包含 `database.path`（保持 `/data/digest.db`，对应现有数据卷）、`delivery.enabled`、`anthropic.api_key`、`anthropic.model`、可选的 `anthropic.base_url` 和 `arxiv.lookback_days`（1～30 天）。程序不会从旧环境变量补齐这些字段；SDK 也只使用配置中的 API key 和服务地址。直接运行 CLI 时默认读取工作目录下的 `config/config.json`，如需其他路径可在子命令前指定 `--config <文件>`。`health` 和 `preview` 不读取该文件。

使用兼容 Anthropic Messages API 的本地网关时，将 `anthropic.base_url` 填为服务根地址（不额外添加 `/v1`），并配置网关的密钥和模型名称。宿主机直接运行 CLI 可用 `http://127.0.0.1:3425`；Docker Desktop 容器应使用 `http://host.docker.internal:3425` 访问宿主机。`base_url` 留空时使用默认 Anthropic API 地址。配置后仍保持 `delivery.enabled=false`，直至完成飞书试发和投递确认。

从旧 `.env` 迁移时，将 `ANTHROPIC_API_KEY`、`ANTHROPIC_MODEL`、`ARXIV_LOOKBACK_DAYS`、`ENABLE_DELIVERY` 分别填入上述 JSON 字段；将 `FEISHU_WEBHOOK_RAS`（旧版可能为 `FEISHU_WEBHOOK_URL`）的地址填入页面「主题管理」中对应主题的推送配置。不要把 URL 或 API key 粘贴进示例配置或提交到 Git。旧 `.env` 不再被使用，若本机存在请自行安全处理。修改实际 JSON 配置后用 `docker compose up -d --force-recreate paper-digest` 重建容器以重新挂载配置（不重建镜像、不删除数据卷）；页面修改 Webhook 无需重建。代码或 Compose 变更才需要重建镜像。

页面支持按 `topics[].id` 管理多个主题的阅读数据与 Webhook。阅读和设置接口均通过 `?topic=<主题 ID>` 明确主题，未知主题返回错误；省略参数时优先联合主题，否则使用配置中的第一个主题。`GET /api/topics` 仅返回主题 ID、显示名称与该主题自动运行状态，不包含 Webhook 地址。配置为空时页面提示登记主题，不会创建隐含的默认配置。

自动采集、筛选、生成和调度目前仍只支持 `recommendation-advertising-search` 联合主题；登记其他主题或保存其 Webhook 不会启动该主题的自动任务，设置页会显示实际状态。将推荐、广告和搜索拆成独立生成主题，仍需对应的采集、筛选和调度规则。其他已登记主题可浏览已有数据，也可用 `send-test --topic <主题 ID> --confirm` 进行受控试发。

## 受控飞书试发

先在飞书确认目标群和群机器人身份，将联合主题机器人的 HTTPS Webhook 保存到「主题管理」中联合主题的推送配置，**保持 `delivery.enabled=false`**。本命令不需要模型密钥、不抓论文，读取数据库中保存的当前地址，只发送以下固定文本：

> 【论文日报机器人连通性测试】这是一条人工触发的测试消息，不是正式论文日报；未调用模型，也未整理真实论文。

确认群、身份和以上内容后，在容器已运行的情况下执行：

```bash
docker compose exec -T paper-digest paper-digest send-test --topic recommendation-advertising-search --confirm
```

此命令会产生**一条真实群消息**，但不会启动日报任务；不用在命令行粘贴 Webhook。只有飞书明确返回成功码才报告请求已被接受，仍须在目标群核对。试发未确认（超时、断网或异常响应）时，先到群里核对，**不要盲目再次执行**；该人工试发不改变日报状态。

仅当 `config/config.json` 中的 `anthropic.api_key` 与页面中的 Webhook 均就绪，且已核对试发对象与内容，才将 `delivery.enabled` 改为 `true` 并重新创建容器。开启自动运行需要模型密钥，且 `topics` 必须登记 `recommendation-advertising-search`；缺少 Webhook 时仍可生成日报，但不执行推送，窗口结束后记为 `missed`，不会自动补发。真实调用模型会产生费用。Webhook URL 包含密钥，实际配置、数据库与备份不能提交或公开。

## 运行语义

- 08:00～09:00 创建或恢复当天任务；单 worker 逐篇生成，最多 5 篇。重启后在 09:00 前继续；09:00 后启动不会自动补发。
- 09:00 仅发送 `ready` 的完整日报，每篇论文一张飞书 Markdown 卡片；空日报发送一条提示。未就绪记 `missed`。发送前持久化整批意图，每篇确认成功后立即记录推荐历史，全部成功才记 `sent`；中途失败停止后续发送并记 `unknown`，**不自动重发整批**，须先去目标群核对。
- 论文 ID 使用不含 arXiv 版本号的稳定 ID；只有确认送达后才记已推荐。生成依据是公开原摘要，日报会明确标注，不能视为论文全文解读。
- `Asia/Shanghai` 在程序中明确指定，不依赖容器时区环境变量。若机器休眠/断电/断网，不能保证 09:00 送达；请自行监控日志中的 `missed` / `unknown` 并保持 Docker Desktop 开机。

## 备份与恢复

SQLite 使用 WAL；**不能仅复制运行中的单个 `.db` 文件**。用内置 `backup` 命令生成一致性快照，再复制到宿主机安全位置：

```bash
docker compose exec paper-digest paper-digest backup /data/digest-backup.db
docker compose cp paper-digest:/data/digest-backup.db ./digest-backup.db
# 核对宿主机备份后清理容器中的临时文件
```

备份文件包含论文摘要、发送历史和保存的 Webhook 密钥，不要上传至公开仓库。恢复时先停止容器，再将备份复制回数据卷内的 `/data/digest.db`，然后启动容器并检查 `status`；恢复演练应在隔离卷中进行，避免覆盖唯一副本。升级数据库结构前先备份并保留前一个镜像/代码版本以便回滚。未来若需要多人权限、独立 Web API 或 Supabase，可迁移到 PostgreSQL；Cloudflare Tunnel 本身不要求迁移数据库。

## 结构

- `internal/config/`：运行配置与主题 Webhook 的加载和校验，示例文件为 `config/config.example.json`。
- `internal/papers/`：arXiv 公开元数据抓取、联合主题筛选和稳定 ID。
- `internal/digest/`：基于公开摘要的 Claude 分析与中文日报组装。
- `internal/state/`：SQLite 事务、逐篇恢复、发送意图与历史去重。
- `internal/job/`：北京时间调度与任务执行。
- `internal/delivery/`：飞书群机器人 Webhook 发送和响应判定。
- `internal/web/`：论文与日报查询 API、Webhook 设置 API、HTTP 健康检查和静态资源服务。
- `web/`：论文库、详情阅读、日报历史与 Webhook 设置的 React 页面。

本项目是 `repos/` 下的独立 Git 仓库；请始终在本目录内操作 Git，不要将其内容加入 `personal-workspace` 主仓库历史。创建公开远端仓库和推送由所有者另行决定。

2026-10-07 的消息格式修复、本机服务更新及停推证据见 [排查记录](docs/2026-10-07-delivery-investigation.md)。

摘要要点统一为 `- **标签**：正文`。新生成摘要使用 `arxiv-summary-v2` 提示；历史摘要在组装日报和发送卡片时自动补齐开头标签加粗，保留正文、链接、代码与公式，不改写历史摘要存档。公式完整性校验跳过 Markdown 代码和链接目标。

发送前会预检整批卡片的实际 JSON 请求体；任一卡片超过 20KB 时整批不发送，日报保持 `ready`，不会因本地大小校验失败进入 `unknown`。
