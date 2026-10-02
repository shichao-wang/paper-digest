# 版本论文库流水线

论文库按 `source + stable paper ID + vN` 保存独立版本。分类召回 `cs.IR`、`cs.LG`、`cs.AI`、`cs.CL`、`stat.ML`；推荐、广告、搜索三主题取并集。关键词只作为标题、摘要综合判断的信号，不补搜索、不设硬命中条件。采集、筛选、全文解析没有业务篇数上限，浏览分页只影响显示。

## 运行与配置

`library.collect_enabled` 与 `library.process_enabled` 分别控制服务中的周期采集和任务消费，默认关闭，与旧 `delivery.enabled` 独立。处理需要模型密钥，不需要飞书 Webhook。旧日报启用时也会在启动监听前校验 Chat 配置，迁移不兼容时直接失败。`library.document_dir` 是数据库引用的源响应、PDF、提取文本共同根目录；容器配置为 `/data/library`，与数据库同在持久卷。宿主机使用本地可写目录。省略文档目录时默认保存到数据库所在目录下的 `library`；`file:` SQLite URI 先解析实际路径并去除查询参数，内存库默认使用当前目录下的 `library`。

其余配置：`categories` 五分类；`concurrency=1`（1～8）；`poll_seconds=900`（至少60秒）；`max_requests=120`（1～1000）、`max_tokens=500000`、`task_timeout_seconds=1800`（60～7200秒）。`max_tokens=0` 使用默认值；显式正值必须能够预留一次最小筛选请求，否则加载配置时拒绝，包括关闭自动处理的配置（仍可手动执行 `process`）。下限由实际系统提示、筛选说明、JSON schema、空元数据和模型名称共同计算，随提示变化更新；当前默认模型 `deepseek-flash` 为8514，`group/deepseek-v4-1-flash` 为8580。4096 只是单次 JSON 输出上限，4096 或5120 的累计预算都不足以发送首请求。

预算是每个模型阶段任务的累计上限，包含所有分块、汇总、修复和重启；增加配置不会补回已有任务的消耗。调用前共享计算 `len(JSON(Request)) + 1024 + 6*len(model) + MaxTokens` 的保守预留（长度按 UTF-8 字节），并先持久化。配置下限只保证空元数据的最小首请求可发送；较长标题、摘要、关键词、正文与追加历史需要更大预算，也不保证单个任务能够完成。可靠 usage 替换该次预留，无可靠 usage 的请求保留预留；因此预算可能提前暂停。后续请求超过剩余额度时保留检查点并暂停，不增加单次输出、不自动扩额或重置累计消耗。费用金额不从 token 推算。

CLI 使用 `--config <文件>` 放在命令之前：

```bash
paper-digest --config config/config.json collect
```

```bash
paper-digest --config config/config.json process
```

```bash
paper-digest --config config/config.json process --id 2610.00001 --version v2
```

```bash
paper-digest --config config/config.json process --batch cs.IR/2026-10-01
```

```bash
paper-digest --config config/config.json library-status
```

`collect` 获取最新可用官方公告，首次不回填历史；已有批次通过 `process --batch` 消费。它不会用最新 feed 冒充指定历史批次。`process` 消费当前可执行队列后返回，未来退避任务留待再次运行；单篇失败不阻塞其他论文。手动命令可以在自动开关关闭时执行。

```bash
paper-digest --config config/config.json retry 123
```

`retry` 继续原任务，保留检查点、已校验分块及预算。预算耗尽后原任务不会因普通重试恢复额度，需要显式新代次：

```bash
paper-digest --config config/config.json reanalyze 2610.00001 v2
```

新代次从精确元信息阶段重新开始，再筛选和解析；因此原元信息仍在排队、重试或运行时也不会跳过校验。筛选明确变为不相关或待判断时，撤销旧分析与比较的可见指针，历史输出仍保留。领取时跳过已被更高代次取代的任务，避免旧下载或模型调用；此前已领取的旧任务可以保存历史结果，但不再推进后续处理或覆盖可见结果。上述处理和重解析会产生模型调用，命令本身不发送飞书。

## 来源与完整性

公告日期、feed 构建时间、采集时间和 API 首次／本版提交时间分开保存。公告 Atom 按分类获取，版本化条目跨分类合并后入队；同时对照官方同日列表稳定 ID 全集、总数与分组计数。官方列表不提供 vN，因此只证明稳定 ID 集合，不声称列表独立核验精确版本。

来源状态包括 `complete`、`incomplete`，发现未捕获的中间公告时记录 `gap`。空 feed 需要同日官方零计数证据。截断、计数／日期不一致、未知结构、同 ID 多版本歧义和资源边界都不能标为完整。已捕获候选及原响应仍留档；无法确认公告日的观察保持日期为空，不造当天日期。当前来源不能可靠恢复任意历史修订公告。gap 检测按通常公告工作日保守提示，节假日等仍需来源核实。

同分类／日期已验证的 `complete` 快照保留权威，后续失败轮询单独留为观察，继续累计响应依赖，不覆盖原计数或候选。元信息阶段按精确版本 `id_list` 核对，保留原响应和 hash；后续未验证公告只补来源及批次关联，不覆盖已核实的描述字段。公共 arXiv 请求共享至少三秒的起始间隔；等待限速不消耗 HTTP 客户端的传输超时，调用方取消与总截止时间仍约束等待和传输。对照前版标为 `comparison_reference`，不伪造公告，也不自动作为新候选排队。

## 阶段与恢复

阶段依次为 `metadata → relevance → document → analyze → compare`。初筛 `direct` 才获取全文；`unrelated`、`uncertain` 保存理由。全文判断可以修正相关性，两次记录均保留。每个版本独立处理；v1 比较不适用，后续版仅比较直接上一版 v(n−1)。比较失败、暂停或前版暂不可得时，本版已经校验的研究结果仍可见。

任务状态：`queued`、`running`、`retry_wait`、`paused`、`blocked`、`succeeded`。短事务领取任务，唯一 token 和到期租约约束所有检查点／结果写入；网络、提取和模型在事务外。阶段结果、有效指针、下阶段入队和成功状态原子提交。过期领取者不能覆盖新运行；generation 防止迟到旧结果覆盖较新代次。比较任务按所属代次读取不可变分析，模型调用前固定当前版与前版文档，重试复用这些输入，避免新代次更新指针后串用材料。关闭服务先取消工作，再等待 worker 和 HTTP handler 结束才关闭数据库。

SQLite 通过显式增量 migration 升级。首次打开有旧数据的库，迁移前自动 `VACUUM INTO` 独占创建 `<数据库路径>.pre-library-v1.db` 一致性快照，失败不迁移。迁移失败重启时核验已有快照的完整性、旧 schema 与数据后复用，不反复生成完整副本；损坏或不匹配的快照保持原样并阻止迁移，需人工核实后恢复或移走。此前随机名称的备份保留，升级后最多额外创建一个稳定名称快照。旧日报四表、摘要及 `sent/unknown` 保留；真实 vN 仅导入 `legacy_digest` 元信息，旧 hash 不冒充 arXiv 版本，不自动排全文任务。

## 全文与 Agent

固定版本 PDF 使用 Go 下载；镜像中的 Poppler `pdfinfo` 计页、`pdftotext -layout -enc UTF-8` 逐页提取。PDF、逐页文本和 manifest 以 hash 标识不可变保存。块连续覆盖全部持久文本，含附录和参考文献；定位为1-based页码及页内 UTF-8 字节偏移，end-exclusive。加载核对源文件、文本、manifest、块、身份与 hash。质量不合格时阻塞，禁止摘要降级。

工具参数只能指向当前登记文档／块／定位，不能接收任意路径、URL 或命令。每块先实际工具阅读全文，再提取结构并本地校验；已读记录与已校验分块分开保存。全部正文块处理完成后汇总。支持固定范围全文块读取、搜索、局部区间和表格文本核查。比较任务分别覆盖当前版和直接前版。

Chat 工具阶段保留完整 assistant、逐调用结果及每块每轮用量和终态，正常结束后独立 `json_object` 生成；本地递归校验必填、类型、可空、数组、额外／重复键，至多两次修复。身份、model、prompt、时间和文档 hash 由程序填写。引用必须精确匹配已处理持久文本，数值与指标、数据集、方法、单位和设置在局部证据一起核对。作者事实、局限与 Agent 评估／应用推断分别保存，未知项保持 null／空列表及 missing_fields。

逐页文本、全块处理轨迹和精确引用证明材料覆盖与程序约束；不能证明模型理解质量，也不能证明 PDF 复杂表格和公式与文本视觉等价。可检测的空页、乱码、过低文本和损坏会阻塞；真实科研内容仍须按原论文核查，无法可靠读出的关键结果不得推断补齐。

## 页面与 API

页面以论文阅读为中心：默认显示精选（直接相关）版本，可切全部论文；同稳定 ID 的不同 vN 各占一行。支持搜索、主题筛选和分页。列表展示标题、作者、摘要和主题；详情展示总结、研究／实验、作者应用与 Agent 推断、原文证据和版本差异。相关性理由与历史、来源完整性、原文质量、任务、预算和运行轨迹保存在后台，不展示在阅读页面。尚无分析或比较时仅显示简短提示。虚构演示明确标记合成数据。

只读接口：

- `GET /api/library/papers?q=&batch=&topic=&relevance=direct|all|unrelated|uncertain|pending&status=&page=&pageSize=`
- `GET /api/library/papers/detail?id=…&version=vN`
- `GET /api/library/status`
- `GET /api/library/evidence?id=…&version=vN&document=…&block=…`

状态筛选可用 `paused` 或 `analyze:paused`。证据展开核对完整持久文档，使用独立的两分钟处理与写入期限，调用方更早的取消／截止时间仍生效；其他 API 保持五秒处理期限。查询不会启动模型。列表／详情不返回检查点会话或租约 token，正文按需读取。旧日报 API 保持；`paper+paperDate` 深链仍读取当天摘要快照，`view=digests|legacy` 可浏览历史。旧摘要／日报使用 URL 参数 `digestTopic` 保存配置主题 ID，并向旧 API 传递 `topic`；新版本库的 `topic` 仍是论文研究主题标签，互不混用。旧链接中的配置主题参数仍兼容。

「日报主题管理」提供已登记主题的 Webhook 设置入口，保存到 SQLite，不影响只读版本库查询。`PUT /api/settings/webhook` 依据直接连接的 `RemoteAddr` 校验来源，不信任 Host 或任何转发头：未配置管理令牌时仅允许回环 IP；启动环境的 `PAPER_DIGEST_SETTINGS_TOKEN` 非空时，所有来源都须提供 `Authorization: Bearer <管理令牌>`，以固定长度摘要恒定时间比较。有效令牌仍须通过回环 Host、Origin 和输入校验。响应不返回已存地址或管理令牌；保存／清除不发送群消息。Docker 端口转发通常呈现非回环网关来源，需显式透传该环境变量；本机反向代理部署也必须配置令牌，避免远端用户经代理呈现回环来源。页面令牌仅存在于当前设置页内存，不写入浏览器存储。具体启用方式、链路约束与旧地址迁移见 [README](../README.md#主题与机器人配置)。

## 备份恢复

旧 `backup <文件>` 只保存 SQLite，不足以恢复全文库。完整入口先做数据库一致性快照，再从该快照枚举所引用不可变源响应、文档与依赖，核 hash 后原子发布 manifest：

```bash
paper-digest --config config/config.json backup-library data/library-backup
```

```bash
paper-digest restore-library data/library-backup data/library-restored
```

目标必须未存在；恢复到新目录，检查 SQLite integrity、外键与所有文件 hash，不启动采集、模型或投递。恢复布局为 `database.db`、`artifacts/`、`manifest.json`；验证后用隔离配置指向恢复的数据库与 artifacts 根。运行中的单个 `.db` 文件不能代替一致性快照，恢复不能覆写唯一副本。

## 隔离演示

准备无凭据配置，三个自动开关都关闭，数据库和文档根指向未存在目录，再运行：

```bash
go run ./cmd/paper-digest --config data/web-preview/config.json demo
```

```bash
go run ./cmd/paper-digest --config data/web-preview/config.json serve --listen 127.0.0.1:18081 --web-dir web/dist
```

演示包含超过一页候选、同 ID 多版本、完整结构／证据、失败／暂停与旧日报历史，不访问 arXiv、模型或飞书。`testdata/seed-web-preview.py` 是旧日报演示工具，不能代替新的全文库 seed。

前置证据见 [arXiv 验证](arxiv-validation.md)、[Chat 协议验证](chat-validation.md)。本轮实现验收记录见 [流水线验收](library-validation.md)。定时推送和用户主动请求的篇数、排序、调度、入口仍是后续工作。
