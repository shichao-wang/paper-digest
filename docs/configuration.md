# 配置与主题

[文档导航](README.md) · [开发](development.md) · [部署与运维](operations.md)

## 配置文件与字段

所有命令示例均从仓库根目录执行。程序严格解析单个 JSON 对象，未知字段、重复主题 ID 和不合法字段会报错；不读取 `.env` 或模型 SDK 的环境默认值。

| 字段 | 示例 / 默认行为 | 校验与用途 |
| --- | --- | --- |
| `database.path` | Compose：`/data/digest.db`；本地：`data/local/digest.db` | 打开数据库时要求非空；相对路径基于进程工作目录，父目录必须先创建 |
| `delivery.enabled` | 示例为 `false` | 控制整个采集、模型生成与推送 worker；关闭时仍提供页面与 API |
| `anthropic.api_key` | 空字符串 | 启用 worker 时必须非空；从配置读取 |
| `anthropic.model` | 示例及代码回退为 `claude-opus-5` | 按所用服务实际支持的模型填写；仓库示例不保证服务端可用性 |
| `anthropic.base_url` | 空字符串 | 使用 SDK 默认地址；自定义地址要求 HTTP/HTTPS、无 URL 凭据、查询参数或片段 |
| `arxiv.lookback_days` | `7` | 始终校验为 1～30；按论文首次发布日期筛选，采集会分页覆盖该窗口 |
| `topics[].id` | `recommendation-advertising-search` | 至少一个，唯一，以小写字母开头，后续用小写字母、数字和分隔用连字符 |
| `topics[].webhook_url` | 新配置省略 | 仅用于旧配置迁移；新地址通过页面保存到 SQLite |

使用 [Compose 示例](../config/config.example.json) 或 [宿主机示例](../config/config.local.example.json)；对应的实际配置 `config/config.json`、`config/config.local.json` 均被 Git 和 Docker 构建忽略。配置没有热加载，Webhook 通过数据库动态读取。直接运行 CLI 默认读取工作目录下的 `config/config.json`，其他路径在子命令前指定 `--config <文件>`；`health` 和 `preview` 不读取配置。

## 配置加载

`config/config.example.json` 是可提交的示例，`config/config.json` 是 **Git 和 Docker 构建都忽略的实际配置**；后者通过 Compose 只读挂载到容器，不在镜像中。请在启动 Compose **之前**创建该文件，且仅在实际配置中填写模型密钥。当前 `recommendation-advertising-search` 是 Recommendation／Advertising／Search 联合主题。

## Webhook 页面配置

飞书群机器人 Webhook 在「主题管理」中配置，点击目标主题所在行的「编辑推送配置」。每个主题独立保存地址；保存、替换或清除一个主题不会修改其他主题。粘贴完整 HTTPS 地址并保存后，地址保存在 SQLite 数据卷中，后续推送每次读取当前配置，无需重启容器。已保存的地址不回显；替换时填写新地址即可。「清除配置」经过确认后暂停当前主题推送，重新配置后恢复。保存、替换和清除均不会发送群消息，也不会补发历史日报。页面没有登录功能，设置写入仅接受 `localhost` 或回环 IP 的 Host，请从本机地址打开设置。代理需保留原始 Host；域名入口不能用于修改 Webhook，对外开放阅读页面时仍需配置访问控制。

## 旧 Webhook 迁移与权限

已有 JSON 中的 `topics[].webhook_url` 仍兼容：`serve` 和已确认的 `send-test` 首次打开数据库时，将合法的旧地址迁入数据库，已有页面配置不会被覆盖，已清除的地址也不会复活。迁移后可从实际 JSON 中移除 `webhook_url`。数据库与备份包含 Webhook 密钥，应按凭据保管。服务打开数据库及保存 Webhook 前会将数据库、已有 WAL/SHM 收紧为 `0600`，新数据库与备份也以 `0600` 创建；无法设置权限时会报错停止操作。

## 模型网关

使用兼容 Anthropic Messages API 的本地网关时，将 `anthropic.base_url` 填为服务根地址（不额外添加 `/v1`），并配置网关的密钥和模型名称。宿主机直接运行 CLI 可用 `http://127.0.0.1:3425`；Docker Desktop 容器应使用 `http://host.docker.internal:3425` 访问宿主机。`base_url` 留空时使用默认 Anthropic API 地址。配置后仍保持 `delivery.enabled=false`，直至完成飞书试发和投递确认。

## 旧环境变量迁移与重载

从旧 `.env` 迁移时，将 `ANTHROPIC_API_KEY`、`ANTHROPIC_MODEL`、`ARXIV_LOOKBACK_DAYS`、`ENABLE_DELIVERY` 分别填入上述 JSON 字段；将 `FEISHU_WEBHOOK_RAS`（旧版可能为 `FEISHU_WEBHOOK_URL`）的地址填入页面「主题管理」中对应主题的推送配置。不要把 URL 或 API key 粘贴进示例配置或提交到 Git。旧 `.env` 不再被使用，若本机存在请自行安全处理。修改实际 JSON 配置后用 `docker compose up -d --force-recreate paper-digest` 重建容器以重新挂载配置（不重建镜像、不删除数据卷）；页面修改 Webhook 无需重建。代码或 Dockerfile 变更需要 `--build`；只修改 Compose 运行参数通常只需重新创建容器。

## 多主题与自动任务限制

页面支持按 `topics[].id` 管理多个主题的阅读数据与 Webhook。阅读和设置接口均通过 `?topic=<主题 ID>` 明确主题，未知主题返回错误；省略参数时优先联合主题，否则使用配置中的第一个主题。`GET /api/topics` 仅返回主题 ID、显示名称与该主题自动运行状态，不包含 Webhook 地址。`config.Load` 要求至少登记一个主题；空 `topics` 会阻止 CLI 启动，不会创建隐含的默认主题。

自动采集、筛选、生成和调度目前仍只支持 `recommendation-advertising-search` 联合主题；登记其他主题或保存其 Webhook 不会启动该主题的自动任务，设置页会显示实际状态。将推荐、广告和搜索拆成独立生成主题，仍需对应的采集、筛选和调度规则。其他已登记主题可浏览已有数据，也可用 `send-test --topic <主题 ID> --confirm` 进行受控试发。
