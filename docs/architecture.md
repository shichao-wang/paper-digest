# 运行与目录职责

[文档导航](README.md) · [配置](configuration.md) · [部署与运维](operations.md)

## 数据流与边界

`cmd/paper-digest` 加载 JSON、打开 SQLite、迁移旧 Webhook 并启动 HTTP 服务；`delivery.enabled=true` 时才校验模型密钥、恢复中断发送状态、启动单个 worker。HTTP 服务与 worker 共用 Store，取消与退出时等请求和任务完成后关闭数据库。

worker 在北京时间 08:00～09:00 按一个 arXiv 公告日获取公开元数据。arXiv 在美东 14:00 截止、20:00 公告，公告日是周日到周四；周五和周六不公告。周日公告覆盖周四 14:00 到周五 14:00 的提交，周一公告覆盖周五 14:00 到周一 14:00，周二到周四各覆盖前一个截止时刻到当天 14:00。日报日期 D 取严格早于当天北京时间 08:00 的最新一次公告。夏令时这次公告落在次日北京时间 08:00，冬令时落在次日 09:00，所以都要到下一个北京时间早晨才采集。对应关系在两个时令下相同：周二采集周日公告，周三采集周一公告，周四采集周二公告，周五采集周三公告，周六采集周四公告，周日和周一没有新公告。周日和周一写成空日报，不把上一批再采一遍。相邻公告的提交区间首尾相接，每一分钟只属于一天。替换版本和事后交叉列表仍用原来的提交时间，不会再次进入日报。arXiv 遇美国节日偶尔推迟公告，程序不维护节日表，推迟的那一批不会被下一天补进来。

查询和分档来自该主题的 `selection`；联合主题省略这段时使用内置规则，只匹配标题、摘要和 `cs.IR`，不使用 `all:` 全文检索。查询再限制到该公告日的 `submittedDate`。页间隔 3 秒，最多 5 页只用来阻止这一天失控翻页；窗口未结束就到达该页数时本次采集失败，不保存截断结果。筛选会跳过已确认的推荐历史。内置规则按相关度分档：标题中的推荐、计算广告或检索术语优先；`cs.IR` 还要同时出现 retrieval、ranking、softmax sampling 等技术信号；只有摘要里的强术语时档位较低。单独的 `cs.IR`、孤立的 “search”“ads”“recommendation”，以及 private information retrieval（包括 DNA 存储 PIR）不收集。合格论文全部按相关度、再按首次发布日期排好。粗筛之后，精筛技能把这些候选交给模型，要求每篇给出是否相关、0 到 1 的分数和一句理由。相关论文按分数从高到低，分数相同则保持粗筛顺序，再由 `max_papers`（默认 5）截取前缀。只有这个前缀会用摘要技能生成中文要点并进入飞书日报。精筛在重试后仍失败时，当天不回退到粗筛结果，不发送，并把任务记为 `missed`，原因写在任务的 `message` 里。进程被取消时不记 missed，下次启动可以接着做。模型正常返回但一篇都不相关，则写成空日报。没有粗筛候选时不调用精筛。不会抓取全文、项目页或解读网页。改筛选规则或技能文件只改挂载的文件并重启容器，不需要新镜像；省略技能目录时使用程序内嵌的 `skills/paper-screening` 和 `skills/paper-summary`。

调度只处理当天的北京时间日期，不补停机期间错过的公告日。08:00～09:00 创建或恢复当天任务；09:00 的一分钟内只发送当天已就绪的日报。09:01 起把当天未完成的任务记为 missed，不自动补发，第二天也不再生成这一天。

筛选评估（`paper-digest eval`、`GET /api/eval` 与 `POST /api/eval`）用已加载的规则预览指定的北京时间日报日期。`--date` 是开始日，`--to` 含首尾，最多 14 天。每一天使用上面的公告日映射；没有新公告的日期结果为空。它列出该公告日里全部粗筛收集到的论文、粗筛上限内的前缀，以及精筛之后会推送的那些。已保存的精筛结论会直接显示。`POST` 和 `eval --rules` 额外编译一份草稿规则；`eval --screen` 和页面上的勾选才会调用模型，可另附一份草稿 `SKILL.md`。这些预览不写回配置或技能文件，不发送，也不写入 `jobs`、`job_papers`、`papers`、`screening` 或 `recommendations`。打开页面不会调用模型。宽松基线 `SelectLoose` 只用于对照，不参与日报。人工判断写入 `selection_labels`；精筛结论写入 `screening`。旧数据库在打开时用 `CREATE TABLE IF NOT EXISTS` 建表。预览可能访问 arXiv，因此这次请求的处理时限长于其他 API。有精筛结论时精确率按精筛后将推送的论文计算，否则按粗筛上限内的论文计算。

候选论文、版本和逐篇中文摘要写入 SQLite，全部完成后将日报记为 `ready`。模型摘要保存模型名称和提示版本；新摘要的提示版本是摘要技能名加正文哈希，例如 `paper-summary@0123abcd`。要点格式仍为 `- **标签**：正文`。渲染日报和发送卡片时也会规范历史标签，不改写存档。摘要完整性校验检查数学分隔符并跳过 Markdown 代码和链接目标。模型请求使用 Anthropic Messages 兼容接口，`max_tokens` 为 4096，并开启思考（`budget_tokens` 2048，且必须小于 `max_tokens`）；非 `end_turn` 的结果不保存。精筛每批最多 8 篇，每批最多 3 次，单次请求约 45 秒，并要落在当天 09:00 之前。

09:00 的一分钟内发送完整日报，整批先预检卡片大小、再写入发送意图。每篇确认后立即保存去重历史，整批完成才记为 `sent`；中途失败停止后续发送并记为 `unknown`。未就绪或错过窗口记为 `missed`，不自动补发。详细窗口、重启和状态语义见 [运维指南](operations.md#调度与状态)。

论文库保存每日入选项，**不是全部抓取结果**。按最近入选日期排列并合并稳定 ID；按日期查看时保留当日版本与摘要。未完成中文摘要也可浏览。主题之间按 ID 隔离论文、日报与 Webhook。页面把选中主题放入 URL，支持刷新、前进后退和分享。

## 目录职责

```text
paper-digest/
├── cmd/paper-digest/       CLI、依赖装配、HTTP/worker 生命周期
├── internal/
│   ├── config/            JSON 加载、主题和 URL 校验
│   ├── papers/            arXiv Atom 抓取、稳定 ID、选题和排序
│   ├── eval/              选题预览、基线对照、人工判断精确率与回归样本
│   ├── digest/            模型分析、格式与数学校验、日报/卡片正文渲染
│   ├── state/             SQLite 结构、状态转换、查询、备份与 Webhook 存储
│   ├── job/               北京时间调度、生成、逐篇投递与恢复
│   ├── delivery/          飞书请求体、大小预检和响应判定
│   └── web/               HTTP API、设置校验、健康检查和静态托管
├── web/                   React + TypeScript + Vite 前端
│   ├── src/               页面、hooks、类型和样式
│   └── public/licenses/   前端字体许可证
├── skills/                精筛与摘要技能；可只读挂载，省略时使用内嵌副本
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
| `GET /api/papers/detail?id=...&date=YYYY-MM-DD` | `id` 和 `date` 均必填；按稳定 ID 与日期定位当日版本，缺少或无效日期返回 400 |
| `GET /api/digests` | 分页日报历史 |
| `GET /api/digests/YYYY-MM-DD` | 当天完整日报与论文 |
| `GET /api/settings/webhook` | 仅返回是否已配置及自动任务启用状态 |
| `PUT /api/settings/webhook` | JSON `{"webhookURL":"https://..."}` 保存地址，空字符串清除；不会试发 |
| `GET /api/eval/rules` | 返回当前主题的筛选规则，不访问 arXiv |
| `GET /api/eval/skill` | 返回当前主题的精筛技能正文，不调用模型 |
| `GET /api/eval?date=YYYY-MM-DD&to=YYYY-MM-DD` | 预览粗筛、已保存精筛、宽松基线和当日已保存日报；不调用模型，不写日报或去重记录 |
| `POST /api/eval` | JSON `{"date","to","rules","screen","screeningSkill"}`。`rules` 可省略。`screen` 为 true 时才调用模型，可用草稿技能正文；要求本机同源，不写配置、技能或日报 |
| `PUT /api/eval/labels` | JSON `{"paperID","label":"relevant\|not_relevant\|clear","snapshot"}` 保存人工判断；要求本机同源 |
| `GET /api/eval/fixtures` | 导出已标注样本，角色为 `positive` 或 `negative` |

论文、日报和设置接口均接受 `?topic=<id>`；省略时优先联合主题，否则取配置首个主题，未知主题返回 404。主题名称没有独立 JSON 字段：联合主题显示「推荐 / 广告 / 搜索」，其他主题显示 ID。分页参数和筛选值的校验见查询实现。

设置写入要求 localhost 或回环 IP 的 Host，Origin 若存在须与 Host 同源；接口不回显地址。无登录功能，反向代理对外开放阅读时需配置访问控制，并保留 Host。数据库及备份包含 Webhook 密钥，必须私密保管。
