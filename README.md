# 论文日报

本机 Docker Compose 运行的 Go 服务，提供按 arXiv 公告批次采集的独立版本论文库：全部候选留档，以标题、摘要和关键词判断推荐／广告／搜索相关性，相关论文下载精确版本全文，逐块工具阅读、结构化提取和证据校验，并与直接上一版比较。浏览器聚焦论文总结、研究、实验、应用与版本差异，也可浏览全部论文和历史日报；采集与处理链路记录留在后台。

新库流水线与旧日报调度分别运行，推送细节留待后续设计。旧日报仍是北京时间08:00生成公开摘要要点、09:00发送的历史兼容入口。

> 示例配置的采集、处理和投递默认全部关闭。`library.collect_enabled` 控制公告采集，`library.process_enabled` 控制有预算的模型处理，`delivery.enabled` 控制旧日报。三者独立。部署、真实模型内容质量与飞书投递需分别验收；本轮源码实现未替换已有生产容器。

新的配置、CLI、恢复语义与可读结果见 [版本论文库流水线](docs/library-pipeline.md)，本轮证据见 [流水线验收](docs/library-validation.md)。

## 本机构建

```bash
cp config/config.example.json config/config.json
chmod 600 config/config.json
# 只在 config/config.json 中填真实凭据；首次保持 delivery.enabled=false
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 paper-digest
```

本机页面映射至 **http://127.0.0.1:8080**，仅监听宿主机回环地址；Cloudflare Tunnel 不是日报任务的依赖。三个自动开关关闭时页面仍可浏览已有数据。Docker Desktop、宿主机和网络必须在任务时段持续在线、未睡眠。Compose 仅运行一个副本，切勿扩为多个定时 worker。代码更新后在非 08:00～09:00 时段运行 `docker compose up -d --build`；容器重建不会删除 `digest-data` 数据卷。不要运行 `docker compose down -v`，那会删除数据。

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

启动 Compose 后打开 http://127.0.0.1:8080。新库按版本独立显示，支持搜索、主题筛选，以及精选／全部论文切换。详情显示总结、研究、实验、业务应用、版本比较与证据，原文按需展开。筛选理由、原文质量、任务与运行记录保存在后台，不占用阅读页面。页面只读，不提供重生成或发送操作。

「旧摘要库／日报历史」继续保存旧每日最多5篇的结果和当天摘要；旧 `paper+paperDate` 深链保持当天版本，不将摘要标为全文。空库不会自动填充示例或抓取历史论文。

前端位于 `web/`，使用 React、TypeScript 和 Vite；Docker 自动构建静态资源，由 Go 同源托管。运行时不需要 Node。直接在宿主机开发需要 Node 24 和 Go 1.27：

```bash
npm --prefix web ci
```

```bash
npm --prefix web run build
```

准备独立的开发配置，保持 `delivery.enabled`、`library.collect_enabled`、`library.process_enabled` 全部关闭，将 `database.path` 与 `library.document_dir` 指向本地可写目录。不要将容器中的 `/data/digest.db` 路径直接用于宿主机：

```bash
go run ./cmd/paper-digest --config config/config.json serve
```

另一个终端启动前端，访问 http://127.0.0.1:5173；Vite 将 `/api` 代理至本机 Go 服务：

```bash
npm --prefix web run dev
```

离线查看新版本论文库，先按 [隔离演示说明](docs/library-pipeline.md#隔离演示) 准备无凭据配置，三个自动开关均关闭，数据库和文档目录必须尚未存在：

```bash
go run ./cmd/paper-digest --config data/web-preview/config.json demo
```

```bash
go run ./cmd/paper-digest --config data/web-preview/config.json serve
```

新演示创建 30 个明确标记 `synthetic_demo` 的虚构版本、全文结构与证据、任务状态和旧日报历史，不访问 arXiv、模型或飞书，不覆盖已有目标。演示内容保存在被 Git 忽略的 `data/web-preview/`。`testdata/seed-web-preview.py` 仅用于旧摘要页面，不适用于新全文库。

`serve` 默认监听 `127.0.0.1:8080`，从 `web/dist` 读取资源，可使用 `--listen` 和 `--web-dir` 调整。Go 启动前须先构建静态资源。`health` 无需配置，通过本机 `/api/health` 检查 HTTP 和数据库；服务未运行时返回失败，可使用 `health --url <地址>` 检查其他监听地址。数据库打开和只读查询不会修改发送中的状态；中断发送恢复只在启用日报 worker 时执行。

## 主题与机器人配置

`config/config.example.json` 是可提交的示例，`config/config.json` 是 **Git 和 Docker 构建都忽略的实际配置**；后者通过 Compose 只读挂载到容器，不在镜像中。请在启动 Compose **之前**创建该文件，且仅在实际配置中填写密钥。当前 `recommendation-advertising-search` 是 Recommendation／Advertising／Search 联合主题，其 `webhook_url` 是对应群机器人的 HTTPS Webhook。

配置文件还包含 `database.path`（保持 `/data/digest.db`，对应现有数据卷）、`delivery.enabled`、`anthropic.api_key`、`anthropic.model`、可选的 `anthropic.base_url` 和 `arxiv.lookback_days`（1～30 天）。`anthropic` 是保留的配置兼容名称，当前业务请求统一采用 Chat Completions；程序只使用配置中的密钥和地址，不读取旧环境变量。直接运行 CLI 时默认读取工作目录下的 `config/config.json`，如需其他路径可在子命令前指定 `--config <文件>`。`health` 和 `preview` 不读取该文件。

默认模型为 `deepseek-flash`，默认地址为 `https://api.deepseek.com/v1`，请求 `POST /v1/chat/completions`。旧 Anthropic 型号与空地址组合会在请求前拒绝，不能把旧凭据解释为 DeepSeek 密钥；迁移需明确设置 DeepSeek 型号与对应密钥，或显式配置兼容 Chat 网关地址。已有官方 `https://api.deepseek.com/anthropic` 配置可在内存中转换为同一官方 Chat 地址，实际文件无需改名。使用其他兼容 Chat 网关时，配置根地址或 `/v1` 地址及相应密钥、模型；任意网关的 `/anthropic` 路径不会自动改写。宿主机可用 `http://127.0.0.1:3425`；Docker Desktop 容器用 `http://host.docker.internal:3425`。客户端关闭思考和流式输出，没有自动重试；仅接受正常完成且非空的正文。配置后仍保持 `delivery.enabled=false`，直至完成飞书试发和投递确认。

从旧 `.env` 迁移时，将 `ARXIV_LOOKBACK_DAYS`、`ENABLE_DELIVERY` 填入上述 JSON 字段；模型配置需明确选择服务：使用 DeepSeek 官方时填写对应的 DeepSeek 密钥与型号，使用兼容 Chat 网关时显式填写其地址、密钥与型号。不能只把旧 `ANTHROPIC_API_KEY` 和 Claude 型号复制到空地址配置当作已完成迁移；官方地址也会在请求前拒绝明显的 `sk-ant-*` 密钥。将 `FEISHU_WEBHOOK_RAS`（旧版可能为 `FEISHU_WEBHOOK_URL`）填入相应主题的 `webhook_url`。不要把 URL 或 API key 粘贴进示例配置或提交到 Git。旧 `.env` 不再被使用，若本机存在请自行安全处理。修改实际配置后用 `docker compose up -d --force-recreate paper-digest` 重建容器以重新挂载配置（不重建镜像、不删除数据卷）；代码或 Compose 变更才需要重建镜像。

可以预先添加其他主题的机器人 URL 并对其受控试发，但**新增主题不会启动该主题的日报**：目前只运行上述联合主题；新主题还需另行实现论文抓取、筛选、渲染和调度。

## 受控飞书试发

先在飞书确认目标群和群机器人身份，将联合主题机器人的 HTTPS Webhook 填入 `config/config.json` 对应主题的 `webhook_url`，**保持 `delivery.enabled=false`**。本命令不需要模型密钥、不读数据库、不抓论文；只发送以下固定文本：

> 【论文日报机器人连通性测试】这是一条人工触发的测试消息，不是正式论文日报；未调用模型，也未整理真实论文。

确认群、身份和以上内容后，在容器已运行的情况下执行：

```bash
docker compose up -d --force-recreate paper-digest  # 配置更新后重新挂载文件
docker compose exec -T paper-digest paper-digest send-test --topic recommendation-advertising-search --confirm
```

此命令会产生**一条真实群消息**，但不会启动日报任务；不用在命令行粘贴 Webhook。只有飞书明确返回成功码才报告请求已被接受，仍须在目标群核对。试发未确认（超时、断网或异常响应）时，先到群里核对，**不要盲目再次执行**；该人工试发不写入 SQLite，也不影响日报状态。

仅当 `config/config.json` 中的 `anthropic.api_key`、当前主题的 `webhook_url` 均就绪，且已核对试发对象与内容，才将 `delivery.enabled` 改为 `true` 并重新创建容器。缺少凭据时保持禁用，不能把它视为已上线。真实调用模型会产生费用。Webhook URL 包含密钥，实际配置、数据库与备份不能提交或公开。

## 旧日报运行语义

- 08:00～09:00 创建或恢复当天任务；单 worker 逐篇生成，最多 5 篇。重启后在 09:00 前继续；09:00 后启动不会自动补发。
- 09:00 仅发送 `ready` 的完整日报；未就绪记 `missed`。发送前持久化意图，只有飞书明确成功才记 `sent`；超时、异常退出等记 `unknown`，**不自动重发**，须先去目标群核对。
- 论文 ID 使用不含 arXiv 版本号的稳定 ID；只有确认送达后才记已推荐。生成依据是公开原摘要，日报会明确标注，不能视为论文全文解读。
- `Asia/Shanghai` 在程序中明确指定，不依赖容器时区环境变量。若机器休眠/断电/断网，不能保证 09:00 送达；请自行监控日志中的 `missed` / `unknown` 并保持 Docker Desktop 开机。

## 备份与恢复

SQLite 使用 WAL；**不能仅复制运行中的单个 `.db` 文件**。用内置 `backup` 命令生成一致性快照，再复制到宿主机安全位置：

```bash
docker compose exec paper-digest paper-digest backup /data/digest-backup.db
docker compose cp paper-digest:/data/digest-backup.db ./digest-backup.db
# 核对宿主机备份后清理容器中的临时文件
```

旧 `backup` 命令只含数据库，无法恢复全文文件。新库使用 `backup-library <未存在目录>` 和 `restore-library <备份目录> <未存在恢复目录>`；快照后按数据库文件引用复制与核 hash，恢复不启动任务，详见 [完整备份恢复](docs/library-pipeline.md#备份恢复)。

备份文件可能包含论文摘要和发送历史，不要上传至公开仓库。恢复演练应在隔离目录或卷中进行，避免覆盖唯一副本。首次迁移旧库前程序自动生成不覆盖的一致性快照；仍应保留前一个镜像和完整备份以便回滚。

## 新流程前置验证

公告批次、完整候选库与全文 Agent 已接入独立流水线。协议与来源的前置验证见：

- [arXiv 公告完整性与续接验证](docs/arxiv-validation.md)：五分类当前批次与官方列表核对，以及历史修订恢复的边界。
- [模型通道验证](docs/model-validation.md)：现有网关的多轮工具、错误恢复、结构输出与输入容量样例。
- [DeepSeek 官方结构化输出检查](docs/deepseek-structure-validation.md)：Messages、Chat JSON模式与beta严格工具调用的支持范围和实测限制。
- [官方 Chat 全流程验证](docs/chat-validation.md)：统一 Chat 客户端、工具证据、版本比较、JSON 校验及修复的独立验收。

`cmd/verify-chat` 是当前流程验证入口；`cmd/verify-model` 保留为此前 Messages／SDK 的历史诊断，不供新业务调用。

验证工具独立运行，不打开业务数据库，也不触发飞书发送。真实模型验证必须显式传入 `--live`，会产生模型消耗；具体复跑方法和限制见报告。

## 结构

- `internal/config/`：运行配置与主题 Webhook 的加载和校验，示例文件为 `config/config.example.json`。
- `internal/papers/`：arXiv 公开元数据抓取、联合主题筛选和稳定 ID。
- `internal/digest/`：基于公开摘要的 DeepSeek Chat 分析与中文日报组装。
- `internal/modelchat/`：Chat 客户端、完整工具往返及本地 JSON 校验。
- `internal/library/`：论文版本、结构分析、证据与任务共享合同。
- `internal/document/`：精确版本 PDF、Poppler 逐页提取、不可变文件与哈希校验。
- `internal/analysis/`：可恢复工具阅读、结构提取、证据／数值校验及直接前版比较。
- `internal/pipeline/`：独立公告采集和持久任务消费。
- `internal/archive/`：数据库与文件的一致性完整备份恢复。
- `internal/state/`：SQLite 事务、逐篇恢复、发送意图与历史去重。
- `internal/job/`：北京时间调度与任务执行。
- `internal/delivery/`：飞书群机器人 Webhook 发送和响应判定。
- `internal/web/`：只读论文与日报 API、HTTP 健康检查和静态资源服务。
- `web/`：论文库、详情阅读和日报历史的 React 页面。

本项目是 `repos/` 下的独立 Git 仓库；请始终在本目录内操作 Git，不要将其内容加入 `personal-workspace` 主仓库历史。创建公开远端仓库和推送由所有者另行决定。
