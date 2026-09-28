# 论文日报

本机 Docker Compose 运行的 Go 服务：每天北京时间 08:00 搜集 Recommendation / Advertising / Search **联合主题**的 arXiv 新论文，根据公开摘要生成中文要点；09:00 仅在日报完整就绪时，通过飞书群机器人 Webhook 发送一份日报。第一版不抓取全文、项目页或解读网页；历史论文、摘要版本、任务与发送状态保存在 SQLite 中。

> 默认 **不调用模型、不发送飞书**。须自行确认目标群、机器人身份、内容及模型费用后，显式配置 `ENABLE_DELIVERY=true` 才启用实际任务。现阶段没有做过真实发送或 09:00 投递验收。

## 本机构建

```bash
cp .env.example .env
# 根据需要填写 .env；首次可保持 ENABLE_DELIVERY=false
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 paper-digest
```

不会映射端口；Cloudflare Tunnel 不是日报任务的依赖。Docker Desktop、宿主机和网络必须在任务时段持续在线、未睡眠。Compose 仅运行一个副本，切勿扩为多个定时 worker。代码更新后在非 08:00～09:00 时段运行 `docker compose up -d --build`；容器重建不会删除 `digest-data` 数据卷。不要运行 `docker compose down -v`，那会删除数据。

```bash
go test ./...
go vet ./...
docker compose config
# 仅渲染离线 fixture，不读数据库、不使用密钥、不访问网络
go run ./cmd/paper-digest preview ./testdata/preview.json
# 查看当天任务状态
docker compose exec paper-digest paper-digest status
```

## 主题与机器人配置

`config/topics.json` 只保存主题 ID 到 Webhook **环境变量名**的映射，不保存真实 URL。当前 `recommendation-advertising-search` 是 Recommendation／Advertising／Search 联合主题，映射到 `FEISHU_WEBHOOK_RAS`；真实 HTTPS URL 只存放在本机忽略的 `.env`，不得放进仓库或镜像。从旧配置迁移时，将 `.env` 中的 `FEISHU_WEBHOOK_URL` 改名为 `FEISHU_WEBHOOK_RAS`，不要把 URL 粘贴进 `config/topics.json`。变更映射或 `.env` 后需重建容器。

可以预先添加其他主题的机器人映射并对其受控试发，但**新增映射不会启动该主题的日报**：目前只运行上述联合主题；新主题还需另行实现论文抓取、筛选、渲染和调度。

## 受控飞书试发

先在飞书确认目标群和群机器人身份，将联合主题机器人的 HTTPS Webhook 填入 `.env` 的 `FEISHU_WEBHOOK_RAS`，**保持 `ENABLE_DELIVERY=false`**。本命令不需要模型密钥、不读数据库、不抓论文；只发送以下固定文本：

> 【论文日报机器人连通性测试】这是一条人工触发的测试消息，不是正式论文日报；未调用模型，也未整理真实论文。

确认群、身份和以上内容后，在容器已运行的情况下执行：

```bash
docker compose up -d --build  # .env 更新后需要重建容器，使 Webhook 配置生效
docker compose exec -T paper-digest paper-digest send-test --topic recommendation-advertising-search --confirm
```

此命令会产生**一条真实群消息**，但不会启动日报任务；不用在命令行粘贴 Webhook。只有飞书明确返回成功码才报告请求已被接受，仍须在目标群核对。试发未确认（超时、断网或异常响应）时，先到群里核对，**不要盲目再次执行**；该人工试发不写入 SQLite，也不影响日报状态。

仅当 `ANTHROPIC_API_KEY`、当前主题对应的 `FEISHU_WEBHOOK_RAS` 均就绪，且已核对试发对象与内容，才将 `.env` 中 `ENABLE_DELIVERY=true` 并重建容器。缺少凭据时保持禁用，不能把它视为已上线。真实调用模型会产生费用。Webhook URL 包含密钥，`.env`、数据库与备份不能提交或公开。

## 运行语义

- 08:00～09:00 创建或恢复当天任务；单 worker 逐篇生成，最多 5 篇。重启后在 09:00 前继续；09:00 后启动不会自动补发。
- 09:00 仅发送 `ready` 的完整日报；未就绪记 `missed`。发送前持久化意图，只有飞书明确成功才记 `sent`；超时、异常退出等记 `unknown`，**不自动重发**，须先去目标群核对。
- 论文 ID 使用不含 arXiv 版本号的稳定 ID；只有确认送达后才记已推荐。生成依据是公开原摘要，日报会明确标注，不能视为论文全文解读。
- `Asia/Shanghai` 在程序中明确指定，与容器 `TZ` 环境变量无关。若机器休眠/断电/断网，不能保证 09:00 送达；请自行监控日志中的 `missed` / `unknown` 并保持 Docker Desktop 开机。

## 备份与恢复

SQLite 使用 WAL；**不能仅复制运行中的单个 `.db` 文件**。用内置 `backup` 命令生成一致性快照，再复制到宿主机安全位置：

```bash
docker compose exec paper-digest paper-digest backup /data/digest-backup.db
docker compose cp paper-digest:/data/digest-backup.db ./digest-backup.db
# 核对宿主机备份后清理容器中的临时文件
```

备份文件可能包含论文摘要和发送历史，不要上传至公开仓库。恢复时先停止容器，再将备份复制回数据卷内的 `/data/digest.db`，然后启动容器并检查 `status`；恢复演练应在隔离卷中进行，避免覆盖唯一副本。升级数据库结构前先备份并保留前一个镜像/代码版本以便回滚。未来若需要多人权限、独立 Web API 或 Supabase，可迁移到 PostgreSQL；Cloudflare Tunnel 本身不要求迁移数据库。

## 结构

- `internal/config/`：主题路由配置加载与校验，映射文件为 `config/topics.json`。
- `internal/papers/`：arXiv 公开元数据抓取、联合主题筛选和稳定 ID。
- `internal/digest/`：基于公开摘要的 Claude 分析与中文日报组装。
- `internal/state/`：SQLite 事务、逐篇恢复、发送意图与历史去重。
- `internal/job/`：北京时间调度与任务执行。
- `internal/delivery/`：飞书群机器人 Webhook 发送和响应判定。

本项目是 `repos/` 下的独立 Git 仓库；请始终在本目录内操作 Git，不要将其内容加入 `personal-workspace` 主仓库历史。创建公开远端仓库和推送由所有者另行决定。
