# DeepSeek 官方结构化输出检查

检查时间：2026-10-02 北京时间，实测 UTC 2026-10-01 18:55:21–18:57:30。模型为 `deepseek-flash`，全部直接请求 `https://api.deepseek.com`，无 magpie、CommandCode 或其他中转。本轮新增7次虚构请求，无重试，未修改实际配置或运行服务，未触发日报／飞书。

## 结论

当前官方接口可以输出 JSON 和工具参数，但**本次没有找到实测可依赖的 schema 强约束路径**。Messages 的格式约束未通过此前测试；Chat 的 json_schema 在普通与 beta 接口均返回400；JSON模式只保证格式；文档化的 beta strict 工具调用能校验输入 schema，却在两种冲突测试中输出 schema 禁止的额外字段。

不能把这些结果推广到所有 DeepSeek 模型或今后服务版本。本次没有测 `deepseek-v4-pro`、思考模式开启、流式输出或复杂论文 schema。也不能据此断言 strict 完全无作用：非法schema被服务端拒绝，但实际生成行为与文档宣称的严格约束不符。

## 文档与实测

| 接口／参数 | 官方文档 | 实测 |
|---|---|---|
| `/anthropic/v1/messages`，`output_config.format` | 支持表只列 `effort`，没有 format | 前轮普通提示通过，冲突提示产生JSON之外内容；本轮不重复请求 |
| `/v1/chat/completions`，`response_format.type=json_schema` | response_format仅列text/json_object | HTTP400，错误指向response_format |
| `/beta/chat/completions`，同上 | beta提供strict工具，不等于支持答案json_schema | HTTP400，错误指向response_format |
| `/v1/chat/completions`，`response_format.type=json_object` | JSON Output文档支持；需在提示中要求JSON | HTTP200、stop、合法JSON，但字段为extra/ok/version，违反additionalProperties=false |
| `/beta/chat/completions`，函数strict=true、指定工具 | 文档称工具参数遵守schema | HTTP200、tool_calls、工具名正确；参数含extra/ok/version，违反schema |
| 同接口，strict=true、additionalProperties=true | 每对象必须全部字段required且additionalProperties=false | HTTP400，错误指向additionalProperties，证实schema请求校验启用 |
| 同接口，strict=true、默认auto、正常提示 | strict支持工具调用 | HTTP200，submit_result参数仅ok/version，符合schema |
| 同接口，strict=true、默认auto、官方天气示例形状＋冲突提示 | 官方示例为get_weather，location:string | HTTP200、get_weather调用；参数含extra/location，仍违反additionalProperties=false |

新增7条请求均明确 `thinking={type:disabled}`，max_tokens=2048、stream=false；指定工具符合官方关于禁用thinking的限制。两个冲突strict案例分别覆盖指定tool_choice与默认auto，天气形状不使用enum。由此不能将首次strict失败简单归因于enum、指定工具或thinking开关。仅单次普通提示成功不能证明约束生效。

服务端实际生成token的4条请求usage分别为49/24、315/65、307/54、292/69（input/output），其余3条HTTP400无成功usage。本会话截至本轮共26次真实模型请求，包含7条本轮请求；未测量费用，不从HTTP状态推算是否计费。

## 对论文流水线的建议

> 后续已决定统一采用官方 Chat：工具读取后独立以 `json_object` 生成结果，本地校验并有界修复。当前验收见[官方 Chat 全流程验证](chat-validation.md)。下文保留当时基于 Messages 的建议与协议限制，能力实测表仍有效。

继续保持官方Messages工具读取通道。Agent最终可用一个 `submit_analysis` 工具提交结构对象，让程序直接读取tool input，减少Markdown／前置说明影响；这个提交工具仍需本地完整schema和证据校验，不能将普通tool参数JSON误称为服务端强约束。

结构校验失败后把字段路径和错误返回Agent有界修复，证据／版本不符时补读核对；超出预算标记待重试，校验通过后才入库。正式PaperAnalysis schema需要单独验收，不能用这里两字段样例代替。

如果以后验证beta strict恢复可靠，可接入该官方Chat路径，但需要单独的协议客户端／终态处理以及schema子集兼容。不能把整个系统base_url直接改成 `/beta`：现有程序调用的是Anthropic Messages，`/beta`文档是Chat Completions协议。官方文档对null及某些约束的支持不足以直接承诺PaperAnalysis合同全兼容，本轮未探测这些字段。

## 证据与复跑

可复跑脚本：[verify-deepseek-structure.py](../scripts/verify-deepseek-structure.py)。仅从已有官方配置在内存中读密钥，不复制到临时文件，不输出原始HTTP错误、密钥或原始不合格文本。结果只保留预设虚构字段名、协议状态、schema符合情况与usage。

本机gitignored原始脱敏报告：

- `data/validation/deepseek-structure.json`：完整5条检查。
- `data/validation/deepseek-strict-recheck.json`：两条严格工具复核。
- 前轮 `deepseek-official-schema.json`：Messages测试；见[模型通道验证](model-validation.md)。

从仓库目录运行，实际配置路径按本机情况指定；以下请求会产生真实模型消耗：

```bash
python3 scripts/verify-deepseek-structure.py --config config/config.json --live
```

仅严格工具复核：

```bash
python3 scripts/verify-deepseek-structure.py --config config/config.json --live --recheck-strict --out data/validation/deepseek-strict-recheck.json
```

脚本exit0表示收集并保存完成，不表示各能力通过；需阅读各条http_status、finish_reason、matches_schema。脚本AST语法检查与git diff --check通过；没有重复运行Go／前端检查，因为本轮未改相关代码。

参考官方资料：

- [Anthropic API compatibility](https://api-docs.deepseek.com/guides/anthropic_api)
- [Chat Completion API](https://api-docs.deepseek.com/api/create-chat-completion)
- [JSON Output](https://api-docs.deepseek.com/guides/json_mode)
- [Function Calling／strict Beta](https://api-docs.deepseek.com/guides/tool_calls)
