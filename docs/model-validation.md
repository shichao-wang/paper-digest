# 模型通道验证

验证时间：2026-10-02 北京时间（原始报告 UTC：2026-10-01 17:08～17:12）。本次仅完成计划第一步，不实施论文库流水线。

> 本文保存此前 Messages／SDK 与网关诊断的实测记录。后续按用户决定采用统一 Chat 客户端，当前验收见[官方 Chat 全流程验证](chat-validation.md)；本文中的 SDK runner 建议不再作为新业务接入方案。

## 结论

现有 `group/deepseek-v4-1-flash` 网关通道支持工具型 Agent 所需的基本工具往返。项目锁定的 `anthropic-sdk-go v1.75.0` Tool Runner 实测可用，可以优先复用，不需要为了基本工具循环引入 Agent SDK 或更换模型。

**首次 Messages 路径的 JSON schema 验证未通过；后续检查定位到网关转换丢失格式字段。** magpie 把 Anthropic Messages 转为 CommandCode Chat 时，未将 `output_config.format` 转成 `response_format`。固定同一个后端、改走 Chat 直通后，`json_schema`＋`strict=true` 的普通／冲突提示两次样例均通过。因此首次失败不能归因于后端不支持。修复网关前，现有 Messages 路径仍不能依赖原生约束；程序侧完整 JSON、版本和证据校验始终保留。详见末尾追加诊断。

## 验证方式

- 只从现有配置读取模型名称、API key 与网关地址，未修改配置，不打开业务 SQLite，不调用飞书。
- 原配置地址是容器访问宿主机的 `host.docker.internal:3425`；验证在宿主机运行，显式覆盖为 `http://127.0.0.1:3425`，仍使用同一网关与模型。
- 输入为虚构论文，读取工具只提供固定章节。每次生成不可预先猜测的随机证据代码，最终同时核对版本和代码。
- 最大 4 轮／工具案例、每请求 45 秒、禁用 SDK 自动重试。本次共 11 次请求：工具 8 次、结构输出 2 次、较长输入 1 次。发生真实模型消耗；金额未测量，不能从仅有模型名称换算官方价格。
- 原始报告在 Git 忽略的 `data/validation/model.json` 和 `model-capacity.json`；不含密钥、Webhook、业务论文或账号资料。下表是可提交的结论。

## 实测结果

| 项目 | 结果 | 证据 |
|---|---|---|
| SDK Tool Runner 顺序读取 | 通过 | 3 次请求：读取 method → 读取 results → end_turn；两个随机证据和 v2 均正确 |
| 同轮多个工具调用 | 通过 | 首轮返回 2 个 read_section；程序在一个 user 消息中完整回传；第二轮正常完成 |
| 工具错误后的恢复 | 通过 | 首轮读取不存在章节，程序返回 is_error；模型随后读取两个有效章节并正常完成，明确标记 missing=true |
| 原生结构输出 | 未通过 | 普通提示通过；要求额外字段／说明的冲突提示未遵守 schema，最终结果不是裸 JSON 对象 |
| 较长输入样例 | 通过 | 49,566 字节虚构正文，网关报告 8,124 input tokens；首尾随机证据均正确，正常完成耗时约 5.6 秒 |

工具样例每次请求观察耗时约 2.2～4.9 秒，结构输出样例约 5 秒，没有触发超时。时间是本次端到端观察，不是 SLA。

## 离线验证与复跑

新增命令 [verify-model](../cmd/verify-model/main.go)；不带 `--live` 不会调用模型。需要实际配置路径，密钥不放命令行。以下假定从仓库目录执行，配置已自行准备且模型通道仍对应同一网关：

```bash
go test ./cmd/verify-model
```

```bash
go run ./cmd/verify-model --config config/config.json --base-url http://127.0.0.1:3425 --live
```

完整验证最多 15 次请求，所有案例逐项写结果；有一项失败则退出码 1。本次真实验证首次工具／结构验证返回 1，因为结构约束未通过，这是被验证能力的失败，不是脚本未执行。较长输入追加验证退出码 0。报告目标默认覆盖同路径文件，需保留旧结果时使用 `--out` 指定新路径。

只复跑较长输入：

```bash
go run ./cmd/verify-model --config config/config.json --base-url http://127.0.0.1:3425 --only input_capacity_sample --out data/validation/model-capacity.json --live
```

离线 fake 服务覆盖顺序读取、多工具完整回传、工具错误、追加式历史、非法 JSON／未知字段／null／错误版本／错误随机证据，以及 max_tokens/refusal 不能当成功。`go test ./...` 与 `go vet ./...` 通过。

## 实测边界与下一步约束

- 8,124 tokens 样例仅证明这个输入规模和首尾信息读取；没有验证模型最大上下文、完整论文理解质量、复杂表格／公式或版本差异质量。
- 本次没有真实 PDF、多模态页面、流式协议与超时后恢复测试；实际超时边界未触发，不能据此保证任意长任务完成。
- 真实 refusal/max_tokens 未诱发；离线验证已确保两种结束原因不当作解析成功。
- 一次顺序、多工具、错误恢复成功不保证所有论文上的工具行为都可靠；实现阶段必须保留轮数预算、运行日志、证据校验和可恢复任务。
- 后续采用 SDK runner + 本地 JSON/证据校验。用实际论文进入下一阶段验收前，先完成版本绑定、正文提取质量和证据工具；此步骤不算全文解析验收。

参考：[Anthropic Go SDK](https://github.com/anthropics/anthropic-sdk-go)。本次 SDK 调用核对使用 `claude-api` 技能及当前锁定版本源码；未使用该技能发送任何外部消息。

## 追加：网关与后端诊断

诊断日期：2026-10-02 北京时间；追加模型探测为 UTC 2026-10-01 18:27:26–18:27:42。本轮只读源码、脱敏配置和路由账本，另做三次虚构模型请求，未修改 magpie、实际配置或运行服务。

### 实际路线与丢失位置

- 3425 端口由 `magpie-tier-preview.app` 提供；安装记录对应 `dev-098b922`。目标模型组有 CommandCode、Cline Pass、DeepSeek 三个成员，但第一次两个结构测试的账本均记录 `provider=commandcode`、`model=deepseek/deepseek-v4.1-flash`，HTTP 200，usage 与原验证一致。
- CommandCode 该模型的本机模型缓存列出 `APIs=[chat,responses]`，不含 Anthropic；缓存获取时间早于第一次测试。因此 Messages 请求走协议转换，不能因为 provider 配了 Anthropic 地址就认定直通。
- 对应预览源码 `internal/gateway/anthropic.go:74` 的 `OutputConfig` 只声明 `Effort`，没有 `Format`。JSON 解码忽略 format；通用 `Request` 没有该输出格式字段，`buildChat` 也没有生成 `response_format`。这是已确认的网关转换缺口。main 分支存在同样缺口。
- 普通 Chat→Chat 路径保留原始 `response_format`，因此可用于隔离网关转换与后端能力。原生 Anthropic 直通可保留 format，但不适用于本次 CommandCode 模型路线。

### 固定同后端的实际探测

通过本机 `POST /v1/chat/completions`，明确模型为 `commandcode/deepseek/deepseek-v4.1-flash`，未改组路由。全部三条账本均确认同一后端，未发生 fallback。schema 为两字段对象：`ok:boolean`、`version:enum[v2]`，两字段必填且 `additionalProperties=false`。

| 请求 | 结果 | 证据 |
|---|---|---|
| `response_format.type=json_schema`，`strict=true`，普通提示 | 通过 | HTTP 200，finish_reason=stop，返回 `{"ok":true,"version":"v2"}`，约3.6秒 |
| 同一 schema，提示要求 extra 字段及 JSON 前说明 | 通过 | HTTP 200，finish_reason=stop，仍仅返回符合合同的两字段对象，约2.4秒 |
| `response_format.type=json_object`，相同冲突提示 | 未完成 | HTTP 200，finish_reason=length；1024 completion tokens 全为 reasoning，无正文。不能判断该次 JSON 模式最终结果，也不追加请求 |

前两条分别报告 113/26 和 126/179 input/output tokens；第三条 75/1024。追加共3次真实请求；此前11次，累计14次。未测量金额。脱敏报告 `data/validation/backend-schema.json` 与一次性探测脚本均在 Git 忽略目录，不含密钥。

CommandCode `/models` 的一次只读诊断返回 HTTP 403，未重试或绕过；可用协议结论来自已有本机模型缓存与源码，后端行为结论来自上述三次真实模型请求。API成功并不意味着模型目录访问权限相同。

### 结论与修复方向

此 CommandCode 后端路径在两次 `json_schema strict` 样例上工作；首次失败来自 Messages 转 Chat 时未传递 schema，不能推广成 DeepSeek 模型缺少结构输出。两次样例不能证明所有 schema、全部后端或未来请求都可靠。

推荐在 magpie 的 IR 中保存输出格式，并在 Anthropic→Chat 时生成完整的 `response_format={type:json_schema,json_schema:{name,strict:true,schema}}`；同时覆盖 Responses/Anthropic 重建与组内 fallback，不能静默丢弃或把 json_object 当作同等 schema 保证。用 fake 上游确认字段传递后，再通过原 Messages 验证复测。该网关修复尚未实施。

[DeepSeek 官方 JSON Output](https://api-docs.deepseek.com/guides/json_mode)仅文档化 `json_object`，[strict tool calling](https://api-docs.deepseek.com/guides/tool_calls)是工具参数约束；它们不能用来否定 CommandCode 转接后端实测的 `json_schema` 支持。[CommandCode Provider 文档](https://commandcode.ai/docs/provider)按模型区分 supported_endpoints，但未给出足以保证所有模型结构输出的合同。本地结构、版本、原文证据校验仍须保留。

## 切换到 DeepSeek 官方接口

2026-10-02 北京时间，按用户要求把实际配置改为 `anthropic.base_url=https://api.deepseek.com/anthropic`、`anthropic.model=deepseek-flash`，使用本机已有官方凭据。其他配置字段保持原值，实际配置权限0600，已读回核对。官方 API key 不写报告、临时配置或仓库文件，验证通过内存管道传入 SDK。[官方 Anthropic API 文档](https://api-docs.deepseek.com/guides/anthropic_api)提供此地址和模型名，仅文档化 `output_config.effort`，没有承诺 `format` schema 约束。

切换前对官方直连执行5次虚构请求（顺序工具3次，结构输出2次），没有 magpie 或 CommandCode 中转：

| 验证 | 结果 |
|---|---|
| SDK runner 顺序 method→results→完成 | 通过；v2与随机证据正确，三次请求约1.1/0.7/0.9秒 |
| 官方 Messages `output_config.format` | 未通过；普通提示通过，冲突提示不是裸 JSON，两次均end_turn，约2.6/3.0秒 |

本次结果不能将 CommandCode 的 json_schema 支持推广至 DeepSeek 官方 Messages 接口。切换官方可解决服务来源选择，但没有解决这条接口的答案 schema 约束；本地校验仍必须保留。原始脱敏报告为 `data/validation/deepseek-official-tool.json` 和 `deepseek-official-schema.json`；工具验证exit0，结构验证exit1。累计本会话19次真实模型请求，未测量费用。

独立验证工具新增 `--config -` 从标准输入读取配置（限1MiB），`--only sdk_runner_sequential` 最多4次，`--only native_structured_output` 最多2次；原较长输入入口保留。`go test ./cmd/verify-model`通过。

生效状态：用户明确批准后，现有容器 `paper-digest-paper-digest-1` 已于 UTC 2026-10-01 18:49:16 重启，未重建镜像或删除数据卷。容器内挂载配置与宿主机实际配置一致，官方地址／模型已读回核对，health命令成功且容器healthy。原有delivery开关保持true，本轮没有触发论文生成或飞书发送。

后续官方结构输出专项检查见 [DeepSeek 官方结构化输出检查](deepseek-structure-validation.md)：普通与beta Chat拒绝json_schema、JSON模式允许额外字段、beta strict工具接受有效schema并拒绝非法schema，但两种冲突测试仍产生禁止字段。本次追加7请求，截至此检查累计26请求；尚未实施结果提交／校验流水线。
