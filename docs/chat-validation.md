# 官方 Chat 全流程验证

本轮按“统一改用 Chat 接口，先做完整验证”的要求，使用 `deepseek-flash` 和 `https://api.deepseek.com/v1/chat/completions`。验证范围为模型协议与程序编排闭环；真实论文的PDF提取、全文理解质量、数据库迁移和推送不属于本轮验收。

**最新结论（2026-10-02 北京时间13:20）：官方服务已恢复，10项虚构协议案例都有通过记录。** 恢复后的完整运行9项通过、1项因输出字段语义歧义失败；保留失败结果，修正字段表达后该项单独补测通过。不是一次全套10/10，也不是原生schema强约束或真实论文质量验收。详情见末尾恢复验证。

## 验证合同

全部步骤均为Chat请求：相关性判断、工具阅读、版本对照、最终JSON、校验错误修复。工具调用返回后追加完整assistant消息；每个tool_call_id都返回结果，保留已有正文／证据及reasoning_content。最终生成阶段不再提供读取工具，并设置 `response_format={"type":"json_object"}`。不依赖json_schema或beta strict。

程序校验JSON字段、类型、版本和随机证据，不以HTTP200、JSON合法或模型自称完成作为通过。无工具调用、章节覆盖不足、错误结束原因、空响应、未知工具／非法参数、超时与预算耗尽均不得假报成功。

独立工具使用Go标准库实现已文档化的Chat协议，没有引入新的外部依赖。真实密钥只从实际配置／标准输入在内存读取，不复制到报告或新凭据文件。所有论文与证据均为虚构，真实模型请求有上限且无隐式重试。

## 验证项目

| 项目 | 通过条件 |
|---|---|
| 摘要文本 | 无工具／非JSON的普通Chat请求正常完成，虚构版本与随机证据保留 |
| 相关性判断 | 推荐／广告／搜索取并集；不把关键词命中当硬条件；区分相关研究与误命中 |
| 顺序工具 | method结果返回之后才读results，最终证据与版本正确 |
| 同轮多工具 | 至少一轮多个调用，所有ID对应结果完整回传 |
| 工具错误恢复 | 缺失章节返回错误，模型继续读取有效章节并完成 |
| 正文覆盖 | 虚构短文的全部指定章节被读取，结果使用所提供证据 |
| 版本比较 | 当前v2与直接上一版v1分别读取，双版方法／结果四个证据绑定正确，并核对pointwise→pairwise方法变化及NDCG 0.31→0.38的虚构结果变化 |
| 本地校验与修复 | 先观察不合格JSON被拒，再返回具体错误，通过有界修复 |
| 较长输入 | 约50KB正文首尾随机证据保留；仅是容量样例 |
| 正文指令注入 | 不执行正文中未知工具指令、不改变版本、输出仍绑定工具证据 |

## 验收方式与复跑

不带 `--live` 不会读取真实凭据或发送模型请求。验证工具从已有配置内存读取密钥，以独立文件保存0600报告；不用业务数据库。当前支持官方旧 `/anthropic` 与新 `/v1` 配置，实际均调用 `/v1/chat/completions`。

```bash
go test ./internal/modelchat ./cmd/verify-chat ./internal/digest
```

```bash
go run ./cmd/verify-chat --config config/config.json --out data/validation/chat.json --live
```

部分复核可通过 `--only` 选择固定案例，用 `--request-limit` 设置1～30的共享硬预算（默认30），`--request-timeout-seconds` 设置1～180秒（默认60）。每个案例完成会立即打印固定名称、状态与次数。为保留首轮失败证据，后续用新输出路径；恢复时先单条探测，不直接重复整套：

```bash
go run ./cmd/verify-chat --config config/config.json --only text_summary --request-limit 1 --out data/validation/chat-recheck.json --live
```

报告逐项记录 `passed/failed/inconclusive`、实际请求次数、结束原因、工具名、token usage和耗时；任何项目未通过，命令返回1。请求总数最多30，单请求最多60秒、单案例最多180秒，工具阶段最多4次请求，最终JSON最多1次初始生成＋2次修复。单请求输出默认上限2048 tokens（文本摘要为1200）；这不是任务总token预算，累计输入仍随追加历史增长。较长输入和短虚构正文不能代表任意规模论文或真实科研质量。

业务摘要源代码改为复用同一Chat客户端，保留 `anthropic` 配置兼容名称和旧 `ClaudeAnalyzer` 类型别名。现有官方 `/anthropic` 地址只在内存转为Chat；其他网关该路径被拒绝。默认模型为 `deepseek-flash`。纯摘要请求最大1200输出tokens，单次请求，无自动重试。协议迁移不改变原来仅依据公开摘要生成的内容范围。

源码与运行服务状态分开记录：本轮准备并验证新代码，现有容器未重建／重启，仍运行此前的 Messages 镜像；实际密钥配置与数据卷未修改。本轮没有触发日报或飞书。`cmd/verify-model` 保留历史 Messages 诊断，当前业务及新流程验证使用 Chat。

## 离线验收

全仓库 `go test -timeout 30s ./...`、`go vet ./...` 和 `git diff --check` 已通过；新增客户端、验证命令及业务摘要的 `go test -race` 也通过。fake HTTP 服务验证完整线协议，不用真实密钥：

- 路径、Bearer、关闭thinking、无流式、json_object请求、无工具最终请求。
- 完整assistant原始消息与reasoning_content、同轮两个tool_call_id结果及最终证据历史。
- 未知工具、坏JSON／null／多余参数／重复键不执行；context取消后不继续执行后续工具。
- length、content_filter、refusal、空choices／正文、错误JSON、无tools却tool_calls、HTTP错误、超时与重定向不当成功，错误不泄露响应原文。
- 本地字段／版本／证据校验，观察不合格extra字段→错误反馈→修复通过，以及修复上限与未完成工具阶段。
- 请求预算耗尽时未完成案例记inconclusive，失败案例不阻止后续；报告文件权限0600且拒绝符号链接。
- 三主题正确样例、双版方法／结果变化、文本摘要及长输入首尾信息的fake响应正向检查。

这些测试证明程序处理逻辑；官方模型行为由下一节真实结果单独判定。

## 首轮官方故障记录（保留历史结果）

2026-10-02 北京时间，首轮报告生成于 UTC 2026-10-01 19:42:57。官方模型 `deepseek-flash` 的10个案例全部在首个请求读取正文时达到60秒超时：没有收到可解析的completion、结束原因或usage，命令exit1。实际执行10次HTTP请求，未达到30次上限。不能将这些结果写成模型能力通过，也不能据此认定工具或JSON不支持。

| 项目 | 首轮状态 | 请求数 | 观察 |
|---|---|---:|---|
| 普通文本摘要 | failed | 1 | 正文读取60秒超时 |
| 推荐／广告／搜索相关性 | failed | 1 | 同上 |
| 顺序method→results | failed | 1 | 同上，未取得工具调用 |
| 同轮两个工具 | failed | 1 | 同上 |
| 缺失章节后恢复 | failed | 1 | 同上 |
| 短虚构全文四章 | failed | 1 | 同上 |
| v2与v1比较 | failed | 1 | 同上 |
| 本地校验／修复 | failed | 1 | 同上，未观察真实修复转换 |
| 长输入首尾 | failed | 1 | 同上，输入52,734字节 |
| 正文指令注入 | failed | 1 | 同上 |

首轮客户端将正文读取错误记为 `invalid chat response`；分析全部约60,000ms后发现这是超时误分类，后续已改为正确的超时／取消错误并增加HTTP状态诊断。首轮原始报告保留，不重写为成功或修改后的错误文案。

追加两个最小诊断请求，使用独立Python HTTP客户端、同一官方key和模型：

- 非流式 JSON、max_tokens=64：HTTP200，正文20秒读取超时。
- 流式文本、max_tokens=32：HTTP200，Content-Type为text/event-stream；约60.7秒仅收到5条保活注释，无生成data事件。这只是诊断，没有改为流式实现。

[官方限流说明](https://api-docs.deepseek.com/quick_start/rate_limit)明确繁忙时非流式发空行、流式发 `: keep-alive`，开始推理前可等待至10分钟。随后读取[官方状态页](https://status.deepseek.com)，页面明确显示 **DeepSeek V4.1 Flash API与对话服务性能下降，Investigating**。公开HTML已保存 `data/validation/deepseek-status.html`（SHA256 `7ea1cea00ddd27d07f39b2109eb7dbccf2ed309cac41923e51ac02f193365f78`）。这些证据支持服务降级／排队解释，不能证明具体请求的服务端根因。

本轮共12次真实模型HTTP请求（10次全套、2次独立诊断），此前26次，累计38次。失败请求没有收到usage，费用未测量，不能认定没有计费。确认官方故障后停止追加，未换模型或网关，也未部署运行服务。

首轮脱敏报告位于Git忽略目录：`data/validation/chat.json`、`chat-transport-probe.json`、`chat-stream-probe.json`。首轮结束时官方全流程验收未完成；恢复后的验证记录如下。

## 服务恢复后的验证（2026-10-02）

官方状态页恢复为 **Operational**，无活动故障。北京时间13:13的单条摘要探测收到完整completion：HTTP200、正常stop、2.610秒、1次请求。恢复判断同时有状态页和实际生成结果支持。公开状态快照保存在 `data/validation/deepseek-status-recovered.html` 和 `.json`，采集时间UTC `2026-10-02T05:14:28.743495+00:00`，HTML SHA256为 `e02dd7beec3bb136cb4baef92de8d3f95f6c442d02b4d97ebfe82f4ae247b9e6`。

随后执行完整10项验证：27次请求，案例耗时合计30.215秒，**9项通过、相关性1项失败，命令exit1**。该原始结果保留在 `data/validation/chat-recovered-full.json`。完整运行累计输入20,764 tokens、输出2,198 tokens。

### 相关性字段歧义与针对性补测

原字段 `misleading_keywords` 被用于表达“历史铭文样例是否相关”，预期false；模型返回true，符合该字段通常表示的“关键词是否误导”的含义。增加固定字段的脱敏标签诊断后，观察到其他三类判断、版本及随机证据正确，问题集中于这个字段。

单请求诊断保存在 `data/validation/chat-relevance-diagnostic.json`：首个输出未通过本地相关性校验，修复被1次请求预算阻止，案例状态为 `inconclusive`，不是通过。报告只新增本地校验错误和四个虚构boolean，不保存模型正文、随机证据或凭据。

将字段改为 `historical_abstract_relevant`，明确每个boolean都表示研究问题是否直接相关。四个摘要、三主题并集规则和预期判断均不变。北京时间13:19的单项补测在1次请求、1.616秒内通过，无修复：

| 虚构摘要 | 相关性结果 |
|---|---|
| 无字面主题关键词的个性化排序研究 | true |
| 含三主题词的历史铭文研究 | false |
| 广告竞价、CTR与预算研究 | true |
| 搜索检索与重排序研究 | true |

### 最终验收证据

| 项目 | 完整运行 | 完整运行请求数 | 后续结果／关键观察 |
|---|---|---:|---|
| 普通文本摘要 | passed | 1 | 三个中文要点及版本、随机证据正确 |
| 推荐／广告／搜索相关性 | failed | 3 | 字段语义澄清后单项补测passed，1次请求 |
| 顺序method→results | passed | 4 | method首轮读取，results在后续轮读取 |
| 同轮两个工具 | passed | 3 | 首轮两个参数合法且实际执行的读取，两个ID结果完整回传 |
| 缺失章节后恢复 | passed | 4 | 首轮读取不存在的conclusion，收到错误后读取有效章节 |
| 短虚构全文四章 | passed | 3 | intro、method、results、limitations均读取，四个随机证据正确 |
| v2与直接上一版v1比较 | passed | 3 | 双版四个证据绑定正确，方法及NDCG变化正确 |
| 本地校验／修复 | passed | 2 | 真实观察到extra字段不合格，反馈错误后修复通过 |
| 长输入首尾 | passed | 1 | 52,734字节输入，首尾证据正确，无修复；输入7,649／输出42 tokens |
| 正文指令注入 | passed | 3 | 此虚构样例仅执行允许的读取工具，保留v2及证据 |

因此，10项案例都已有通过证据，来源是完整运行中的9项和字段修正后的1项补测，**不将原完整运行改记为一次10/10成功**。工具阶段均正常结束，再以无tools的独立Chat JSON请求完成结构输出；不能仅凭HTTP200或合法JSON推导通过。

本次恢复验证合计30次实际模型HTTP请求：1次恢复探测＋27次完整运行＋1次字段诊断＋1次澄清补测。合计输入21,479／输出2,382 tokens；连同此前38次，整个会话累计68次请求。费用金额未测量。停止追加模型请求，不重复已通过案例。

证据文件均在Git忽略的 `data/validation/` 中：`chat-recovery-probe.json`、`chat-recovered-full.json`、`chat-relevance-diagnostic.json`、`chat-relevance-clarified.json`；首轮故障文件保留。

本轮最后新增字段语义回归断言后，重新运行全仓库 `go test -timeout 30s ./...`、`go vet ./...` 和 `git diff --check`，全部通过。客户端、验证工具和业务摘要的race检查此前已通过。

验收结论限定为这些虚构协议案例：统一Chat、工具循环、JSON模式、本地校验和有界修复可用。真实PDF提取、完整PaperAnalysis合同和科研内容质量仍需后续验收。业务摘要源码已迁移Chat，现有容器仍为旧Messages镜像，未部署；本轮未访问业务数据库或发送飞书。

参考：[Chat API](https://api-docs.deepseek.com/api/create-chat-completion)、[Tool Calling](https://api-docs.deepseek.com/guides/tool_calls)、[Thinking Mode](https://api-docs.deepseek.com/guides/thinking_mode)、[JSON Output](https://api-docs.deepseek.com/guides/json_mode)。前序能力限制见[官方结构化输出检查](deepseek-structure-validation.md)。
