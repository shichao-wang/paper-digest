# 部署与运维

[文档导航](README.md) · [配置](configuration.md) · [运行与目录职责](architecture.md)

## Docker Compose 部署

需要 Docker Engine 与 Compose 插件，或 Docker Desktop。仓库根目录执行：

```bash
cp config/config.example.json config/config.json
chmod 600 config/config.json
# 仅编辑实际配置，初次保持 delivery.enabled=false
docker compose config --quiet
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 paper-digest
docker compose exec -T paper-digest paper-digest health
```

打开 http://127.0.0.1:8080。Compose 只暴露宿主机回环端口；Cloudflare Tunnel 不是日报任务依赖。镜像通过 Node 24 构建前端、Go 1.27 构建后端，在 Alpine 中以非 root 用户运行，运行时不需要 Node。静态资源在 `/app/web/dist`，实际配置只读挂载到 `/app/config/config.json`，数据库在 `digest-data` 数据卷。

Compose 仅运行一个副本，不要扩为多个定时 worker。宿主机、Docker 和网络必须在任务时段持续在线。外部访问需自行配置访问控制；页面无登录功能，域名入口无法修改 Webhook，详见 [配置](configuration.md)。

## 更新与停用

代码或 Dockerfile 更新：先备份，在非 08:00～09:01 时段执行 `docker compose up -d --build`，再检查健康和日志。容器重建保留数据卷。仅修改实际 JSON 后执行：

```bash
docker compose up -d --force-recreate paper-digest
```

页面修改 Webhook 无需重启。仅更改 Compose 运行参数通常只需 `docker compose up -d`。停用自动任务可将 `delivery.enabled` 设为 `false` 后重新创建容器；这同时关闭采集和模型调用。`docker compose stop` 停止全部服务，`docker compose down` 移除容器和网络、保留命名卷；**不要运行 `docker compose down -v`，它会删除数据**。不同项目名会得到不同命名卷，迁移目录或加 `-p` 前须核对原卷。

## 受控飞书试发

先在飞书确认目标群和群机器人身份，将联合主题机器人的 HTTPS Webhook 保存到「主题管理」中联合主题的推送配置，**保持 `delivery.enabled=false`**。本命令不需要模型密钥、不抓论文，读取数据库中保存的当前地址，只发送以下固定文本：

> 【论文日报机器人连通性测试】这是一条人工触发的测试消息，不是正式论文日报；未调用模型，也未整理真实论文。

确认群、身份和以上内容后，在容器已运行的情况下执行：

```bash
docker compose exec -T paper-digest paper-digest send-test --topic recommendation-advertising-search --confirm
```

此命令会产生**一条真实群消息**，但不会启动日报任务；不用在命令行粘贴 Webhook。只有飞书明确返回成功码才报告请求已被接受，仍须在目标群核对。试发未确认（超时、断网或异常响应）时，先到群里核对，**不要盲目再次执行**；该人工试发不改变日报状态。

仅当 `config/config.json` 中的 `anthropic.api_key` 与页面中的 Webhook 均就绪，且已核对试发对象与内容，才将 `delivery.enabled` 改为 `true` 并重新创建容器。开启自动运行需要模型密钥，且 `topics` 必须登记 `recommendation-advertising-search`；缺少 Webhook 时仍可生成日报，但不执行推送，窗口结束后记为 `missed`，不会自动补发。真实调用模型会产生费用。Webhook URL 包含密钥，实际配置、数据库与备份不能提交或公开。

## 调度与状态

- 08:00～09:00 创建或恢复当天任务；单 worker 逐篇生成，最多 5 篇。重启后在 09:00 前继续；09:01 起不再发起新投递；若在 09:00 的一分钟内启动且已有完整日报，仍可尝试发送。
- 09:00:00～09:00:59 的窗口内只尝试一次，且仅发送 `ready` 的完整日报，每篇论文一张飞书 Markdown 卡片；空日报发送一条提示。未就绪记 `missed`。发送前持久化整批意图，每篇确认成功后立即记录推荐历史，全部成功才记 `sent`；中途失败停止后续发送并记 `unknown`，**不自动重发整批**，须先去目标群核对。
- 论文 ID 使用不含 arXiv 版本号的稳定 ID；只有确认送达后才记已推荐。生成依据是公开原摘要，日报会明确标注，不能视为论文全文解读。
- `Asia/Shanghai` 在程序中明确指定，不依赖容器时区环境变量。若机器休眠/断电/断网，不能保证 09:00 送达；请自行监控日志中的 `missed` / `unknown` 并保持 Docker Desktop 开机。

发送前预检整批卡片的实际 JSON 请求体；任一卡片超过 20KB 时整批不发送，本地预检失败不会进入 `unknown`。此时在窗口内保持 `ready`，窗口结束后按调度规则收口为 `missed`。摘要格式和选题逻辑见 [运行与目录职责](architecture.md)。

```bash
# status 只查看联合主题，默认当天（北京时间）；不触发生成或发送
docker compose exec -T paper-digest paper-digest status
docker compose exec -T paper-digest paper-digest status 2026-10-07
```

| 状态 | 含义与处理 |
| --- | --- |
| `new` / `processing` | 创建任务 / 逐篇生成，08:00～09:00 内可恢复 |
| `ready` | 完整日报已保存，等待发送窗口 |
| `sending` | 已记录整批发送意图；worker 重启恢复时转为 `unknown` |
| `sent` | 整批确认成功，已记录去重历史 |
| `missed` | 错过窗口或窗口内未就绪；不会自动补发 |
| `unknown` | 发送或状态记录未确认；先核对群消息，禁止盲目重发 |

没有当天任务时 `status` 返回错误；健康检查成功只说明 HTTP 与数据库可用，不能证明模型、arXiv、飞书或定时投递成功。`health` 不读配置，默认访问 `http://127.0.0.1:8080/api/health`，自定义监听地址需用 `health --url <地址>`。

## 备份与恢复

SQLite 使用 WAL；**不能仅复制运行中的单个 `.db` 文件**。用内置 `backup` 命令生成一致性快照，再复制到宿主机安全位置：

```bash
mkdir -p backups
umask 077
digest_backup_name="digest-backup-$(date +%Y%m%d-%H%M%S).db"
docker compose exec -T paper-digest paper-digest backup "/data/$digest_backup_name"
docker compose cp "paper-digest:/data/$digest_backup_name" "backups/$digest_backup_name"
chmod 600 "backups/$digest_backup_name"
# 核对宿主机备份后，再删除容器中的临时备份文件
```

备份文件包含论文摘要、发送历史和保存的 Webhook 密钥，不要上传至公开仓库。恢复时先停止容器，再将备份复制回 `database.path` 配置的路径（Compose 默认为数据卷内的 `/data/digest.db`），然后启动容器并检查 `status`；恢复演练应在隔离卷中进行，避免覆盖唯一副本。升级数据库结构前先备份并保留前一个镜像/代码版本以便回滚。

`backup` 的目标文件必须不存在，目标父目录必须已存在。每次使用不同文件名；宿主机副本核对后设为 `0600`，并放在被忽略的 `backups/` 或其他私密位置。恢复前保留当前数据库快照，停止所有打开该卷的进程；处理旧 WAL/SHM、正确的卷名和服务用户属主后再启动。不要直接向正在运行的数据库覆盖文件。

## 故障定位

| 现象 | 首先检查 |
| --- | --- |
| 配置读取失败 | 是否从仓库根目录执行、实际 JSON 是否存在、Docker 挂载是否为文件 |
| 数据库无法打开 / 修改权限失败 | 路径父目录、文件及 WAL/SHM 的属主和可写权限 |
| `static index.html is missing or invalid` | 先构建 `web/dist`，或校对 `serve --web-dir` |
| 页面设置返回 403 | 用 localhost 或回环 IP 访问；代理保留 Host 和同源 Origin |
| 健康但未推送 | `delivery.enabled`、主题 Webhook、当天状态及 08:00～09:01 的主机在线记录 |
| `unknown` | 到群里逐篇核对，部分成功的论文已记去重；不要重发整批 |

[2026-10-07 排查记录](2026-10-07-delivery-investigation.md) 是当日特定本机部署的历史证据，其中端口、镜像标签和未提交改动说明不代表当前仓库部署步骤。
