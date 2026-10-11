# 配置与主题

[文档导航](README.md) · [开发](development.md) · [部署与运维](operations.md)

## 配置文件与字段

所有命令示例均从仓库根目录执行。程序严格解析单个 JSON 对象，未知字段、重复主题 ID 和不合法字段会报错；不读取 `.env` 或模型 SDK 的环境默认值。

| 字段 | 示例 / 默认行为 | 校验与用途 |
| --- | --- | --- |
| `database.path` | Compose：`/data/digest.db`；本地：`data/local/digest.db` | 打开数据库时要求非空；相对路径基于进程工作目录，父目录必须先创建 |
| `delivery.enabled` | 示例为 `false` | 控制整个采集、模型生成与推送 worker；关闭时仍提供页面与 API |
| `anthropic.api_key` | 空字符串 | 启用 worker 时必须非空；从配置读取 |
| `anthropic.model` | 示例为 `deepseek-flash`；配置留空时代码回退为 `deepseek-flash` | 按所用服务实际支持的模型填写。已写明的模型名会照用，不会被示例改掉 |
| `anthropic.base_url` | 示例为 `https://api.deepseek.com/anthropic` | 兼容 Anthropic Messages API 的服务根地址，不要额外加 `/v1`。留空则使用默认 Anthropic API。自定义地址要求 HTTP/HTTPS、无 URL 凭据、查询参数或片段 |
| `agents.screening.model` | 省略 | 省略时用 `anthropic.model`，仍空则是 `deepseek-flash`。与摘要的模型互不影响 |
| `agents.screening.skill` | `paper-screening` | 精筛阶段的默认技能名，从 `skills.dir` 或内嵌技能加载。小写字母开头，只能用小写字母、数字和连字符。不是文件路径 |
| `agents.screening.max_tokens` | `16384` | 精筛单次输出上限，含思考。省略时用此默认。允许 4096～65536 |
| `agents.screening.timeout_seconds` | `180` | 精筛单次请求超时。省略时用此默认。允许 30～600 |
| `agents.screening.batch_size` | `8` | 每批交给模型的论文数。省略时用此默认。允许 1～16 |
| `agents.screening.concurrency` | `2` | 同时进行的精筛批次数。省略时用此默认。允许 1～4 |
| `agents.screening.thinking.budget_tokens` | `2048` | 思考保持开启。省略时用此默认。允许 1024 到该阶段 `max_tokens` 减 1。DeepSeek 忽略这个预算，思考计入 `max_tokens` |
| `agents.summary.model` | 省略 | 同精筛：省略时用 `anthropic.model`，仍空则是 `deepseek-flash` |
| `agents.summary.skill` | `paper-summary` | 摘要阶段的默认技能名，规则同精筛 |
| `agents.summary.max_tokens` | `8192` | 单篇摘要的输出上限，含思考。省略时用此默认。允许 4096～65536 |
| `agents.summary.timeout_seconds` | `120` | 单篇摘要超时。省略时用此默认。允许 30～600 |
| `agents.summary.concurrency` | `2` | 同时生成的摘要数。省略时用此默认。允许 1～4 |
| `agents.summary.thinking.budget_tokens` | `2048` | 同精筛。摘要没有 `batch_size`，写了会拒绝加载 |
| `skills.dir` | 示例为 `/app/skills`，可省略 | 省略时只用程序内嵌的技能。填写时该目录必须存在，其中合法的 `技能名/SKILL.md` 会覆盖同名内嵌技能；文件无效则拒绝启动 |
| `topics[].id` | `recommendation-advertising-search` | 至少一个，唯一，以小写字母开头，后续用小写字母、数字和分隔用连字符 |
| `topics[].webhook_url` | 新配置省略 | 仅用于旧配置迁移；新地址通过页面保存到 SQLite |
| `topics[].selection` | 联合主题省略时使用内置规则 | 该主题的 arXiv 查询、屏蔽、信号、分档和每日上限。见下文 |
| `topics[].skills` | 可省略 | 只替换该主题。`screening` 或 `summary` 非空时覆盖对应的 `agents.*.skill`；省略则用阶段上的技能名 |

使用 [Compose 示例](../config/config.example.json) 或 [宿主机示例](../config/config.local.example.json)；对应的实际配置 `config/config.json`、`config/config.local.json` 均被 Git 和 Docker 构建忽略。配置没有热加载，Webhook 通过数据库动态读取。直接运行 CLI 默认读取工作目录下的 `config/config.json`，其他路径在子命令前指定 `--config <文件>`；`health` 和 `preview` 不读取配置。`eval` 读取数据库路径和主题的 `selection`。没有 `selection` 的旧配置仍能加载。配置里不能再写 `arxiv` 或 `lookback_days`，见下方迁移。

## 筛选规则

`topics[].selection` 是可选对象。`recommendation-advertising-search` 省略它时使用程序内置的推荐、广告和搜索规则，行为与写明示例里的整段规则相同。其他主题省略它则没有筛选规则，筛选评估会拒绝该主题。写了 `selection` 就会在加载时编译：`max_papers` 为 1～20，`min_tier` 为 1～5，查询至少要有一个分类或检索词，正则必须能编译。错误会指出哪一条规则无效，但不会把正则原文打出来。没有单独的采集上限字段；`max_papers` 的含义是每天推送多少篇。

规则在小写后的标题和摘要上匹配。`query.categories` 和 `query.terms`（`field` 为 `title` 或 `abstract`）组成 arXiv 的 `cat:` / `ti:` / `abs:` 查询，不用 `all:`。采集再并上该公告日的 `submittedDate`。页与页之间间隔 3 秒；最多 5 页只是防止这一天无限翻页，窗口还没翻完就碰到这个上限时会报错，而不是悄悄截断。`masks` 先从文本里去掉指定短语，并可以作为排除信号。`signals` 用 `pattern`、`categories` 或 `near` 三者之一；`unless` 指向已经定义、且自身没有 `unless` 的信号。`decisions` 按顺序采用第一条满足条件的分档。低于 `min_tier` 的论文不收集。收集结果按相关度再按首次发布日期排序，不按篇数截断。精筛之后才按模型分数重排，`max_papers` 只决定这个序列的前多少篇会生成摘要并写入当天日报。`SelectLoose` 仍是改规则之前的宽松基线，对照时仍最多 5 篇。

草稿不写配置：`paper-digest eval --rules candidate.json`，或在「筛选评估」里编辑文本框后点「用草稿对照」。`paper-digest eval rules` 只打印当前主题的规则。确认后再把对象放进宿主机配置的 `topics[].selection`，执行 `docker compose up -d --force-recreate paper-digest`。容器只读挂载配置，这一步不构建镜像。

## 精筛和摘要技能

模型面对的说明在 `skills/paper-screening/SKILL.md` 和 `skills/paper-summary/SKILL.md`。每份都是 Agent Skill：YAML 前言含 `name` 和 `description`，后面是 Markdown 说明。`name` 必须和文件夹名一致。摘要技能决定中文要点怎么写，飞书卡片仍使用 `- **标签**：正文`。精筛技能决定粗筛候选里哪些真正相关；输出必须是严格 JSON，运行时会再校验。

`skills.dir` 留空时使用编进二进制的这两份。要改完即生效、又不重建镜像，把目录设成挂载路径。Compose 示例把仓库的 `./skills` 只读挂到 `/app/skills`，示例配置的 `skills.dir` 就是这个路径。宿主机直接运行可以用 `"dir": "skills"`。目录里没有某个技能时仍用内嵌副本；目录里的 `SKILL.md` 无效，或者 `name` 和文件夹不一致，进程会报错退出，不会悄悄退回内嵌文件。阶段默认技能名写在 `agents.screening.skill` 和 `agents.summary.skill`，省略时就是上面两个名字。`topics[].skills.screening` 和 `topics[].skills.summary` 可选，写了就只替换该主题的对应阶段。

精筛失败没有开关：重试耗尽、或生成截止（北京时间 09:00）到达时，当天不发送粗筛结果，任务记为 `missed`，原因出现在 `status`、日报历史和筛选评估里。关掉进程导致的取消不会记成 missed。

评估默认不调用模型。`paper-digest eval --date YYYY-MM-DD --screen` 才会精筛，并可以加 `--skill 草稿.md` 试用一份未保存的技能。页面上对应的是「调用模型精筛」和技能文本框。这两种都只显示结果，不写数据库，也不发送。

## 删掉 lookback_days

`arxiv.lookback_days` 已经从配置里去掉，没有兼容字段。每个北京时间日期只采集一个 arXiv 公告日，不再回看多天。正在使用的 `config.json` 如果还带 `lookback_days` 或整个 `arxiv` 段，进程会拒绝启动，并说明要删掉该字段。`arxiv` 段里没有其他内容时，把整个 `arxiv` 段删掉。改完保存后执行：

```bash
docker compose up -d --force-recreate paper-digest
```

示例配置 `config/config.example.json` 和 `config/config.local.example.json` 已经没有这一段。公告日怎么对应北京时间，见 [运行与目录职责](architecture.md)。

## 配置加载

`config/config.example.json` 是可提交的示例，`config/config.json` 是 **Git 和 Docker 构建都忽略的实际配置**；后者通过 Compose 只读挂载到容器，不在镜像中。请在启动 Compose **之前**创建该文件，且仅在实际配置中填写模型密钥。当前 `recommendation-advertising-search` 是 Recommendation／Advertising／Search 联合主题。

## Webhook 页面配置

飞书群机器人 Webhook 在「主题管理」中配置，点击目标主题所在行的「编辑推送配置」。每个主题独立保存地址；保存、替换或清除一个主题不会修改其他主题。粘贴完整 HTTPS 地址并保存后，地址保存在 SQLite 数据卷中，后续推送每次读取当前配置，无需重启容器。已保存的地址不回显；替换时填写新地址即可。「清除配置」经过确认后暂停当前主题推送，重新配置后恢复。保存、替换和清除均不会发送群消息，也不会补发历史日报。页面没有登录功能，设置写入仅接受 `localhost` 或回环 IP 的 Host，请从本机地址打开设置。代理需保留原始 Host；域名入口不能用于修改 Webhook，对外开放阅读页面时仍需配置访问控制。

## 旧 Webhook 迁移与权限

已有 JSON 中的 `topics[].webhook_url` 仍兼容：`serve` 和已确认的 `send-test` 首次打开数据库时，将合法的旧地址迁入数据库，已有页面配置不会被覆盖，已清除的地址也不会复活。迁移后可从实际 JSON 中移除 `webhook_url`。数据库与备份包含 Webhook 密钥，应按凭据保管。服务打开数据库及保存 Webhook 前会将数据库、已有 WAL/SHM 收紧为 `0600`，新数据库与备份也以 `0600` 创建；无法设置权限时会报错停止操作。

## 模型网关

使用兼容 Anthropic Messages API 的服务时，将 `anthropic.base_url` 填为服务根地址（不额外添加 `/v1`），并配置密钥和默认模型名称。示例指向 DeepSeek 的 Anthropic 兼容地址和 `deepseek-flash`。`anthropic` 只保留这三项。精筛和摘要各自写在顶层 `agents.screening` 与 `agents.summary`：模型、技能名、`max_tokens`、超时、并发，精筛另有 `batch_size`，以及 `thinking.budget_tokens`。阶段上的 `model` 省略时用 `anthropic.model`。不写 `agents`，或只写其中一部分字段，其余用内置默认：精筛 `max_tokens` 16384、超时 180 秒、每批 8 篇、并发 2、技能 `paper-screening`；摘要 `max_tokens` 8192、超时 120 秒、并发 2、技能 `paper-summary`。两边的思考预算默认 2048，必须小于该阶段的 `max_tokens`。思考保持开启。DeepSeek 的 Anthropic 兼容接口会忽略 `budget_tokens`，思考占用的是 `max_tokens` 整段输出额度。已经部署、只有 `anthropic.api_key`、`model`、`base_url` 的配置不用加 `agents` 也能加载。把 `screening` 或 `summary` 写进 `anthropic` 会拒绝启动，并提示改到 `agents`。改完保存后重新创建容器即可，不必重新构建镜像。若响应仍是 `stop_reason=max_tokens`，错误里会写上这次使用的 `max_tokens` 和 `budget_tokens`。宿主机直接运行 CLI 若走本地网关，可用 `http://127.0.0.1:3425`；Docker Desktop 容器应使用 `http://host.docker.internal:3425` 访问宿主机。`base_url` 留空时使用默认 Anthropic API 地址。配置后仍保持 `delivery.enabled=false`，直至完成飞书试发和投递确认。

按上述默认值，约 30 篇粗筛候选是 4 个精筛批次，并发 2，大约两轮。一轮按单次超时计是 3 分钟，并额外留一次重试。随后最多 `max_papers` 篇摘要（默认 5）并发 2，大约三轮，再加一次重试余量。加上采集，通常十几分钟，计划上的上限大约半小时，落在 08:00 开始、09:00 截止的窗口里。启动精筛前会按这个计划检查剩余时间；来不及完成就记为 `missed` 且不调用模型。输出令牌按实际生成计费，`max_tokens` 只是上限。

## 旧环境变量迁移与重载

从旧 `.env` 迁移时，将 `ANTHROPIC_API_KEY`、`ANTHROPIC_MODEL`、`ARXIV_LOOKBACK_DAYS`、`ENABLE_DELIVERY` 分别填入上述 JSON 字段；将 `FEISHU_WEBHOOK_RAS`（旧版可能为 `FEISHU_WEBHOOK_URL`）的地址填入页面「主题管理」中对应主题的推送配置。不要把 URL 或 API key 粘贴进示例配置或提交到 Git。旧 `.env` 不再被使用，若本机存在请自行安全处理。修改实际 JSON 配置后用 `docker compose up -d --force-recreate paper-digest` 重建容器以重新挂载配置（不重建镜像、不删除数据卷）；页面修改 Webhook 无需重建。代码或 Dockerfile 变更需要 `--build`；只修改 Compose 运行参数通常只需重新创建容器。

## 多主题与自动任务限制

页面支持按 `topics[].id` 管理多个主题的阅读数据与 Webhook。阅读和设置接口均通过 `?topic=<主题 ID>` 明确主题，未知主题返回错误；省略参数时优先联合主题，否则使用配置中的第一个主题。`GET /api/topics` 仅返回主题 ID、显示名称与该主题自动运行状态，不包含 Webhook 地址。`config.Load` 要求至少登记一个主题；空 `topics` 会阻止 CLI 启动，不会创建隐含的默认主题。

自动采集、筛选、生成和调度目前仍只支持 `recommendation-advertising-search` 联合主题；登记其他主题或保存其 Webhook 不会启动该主题的自动任务，设置页会显示实际状态。将推荐、广告和搜索拆成独立生成主题，仍需对应的采集、筛选和调度规则。其他已登记主题可浏览已有数据，也可用 `send-test --topic <主题 ID> --confirm` 进行受控试发。
