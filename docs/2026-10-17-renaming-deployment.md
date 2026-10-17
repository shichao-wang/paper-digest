# 2026-10-17 Research Vault 更名部署记录

[文档导航](README.md) · [命名与兼容](renaming.md) · [部署与运维](operations.md)

2026-10-17 北京时间 11:58，在日报调度窗口外完成本机部署切换。代码从本工作树构建，未覆盖主仓库已有的未提交改动。

| 项目 | 切换结果 |
| --- | --- |
| CLI 与二进制 | `research-vault`、`/usr/local/bin/research-vault` |
| 镜像 | `research-vault:rename-20261017` |
| Compose 项目与服务 | 仍为 `paper-digest` |
| 数据卷 | 仍为 `paper-digest_digest-data` |
| 本机入口 | 仍为 `http://127.0.0.1:8081` |
| 实例与健康 | 一个服务实例，容器健康，HTTP 与数据库健康检查通过 |
| 配置和技能 | 挂载来源由旧仓库路径改为新仓库路径，继续只读挂载 |

部署文件位于本工作树被忽略的 `data/rename-deploy/compose.json`，使用时携带 `-p paper-digest -f data/rename-deploy/compose.json`。它保留该部署的 8081 端口和外部命名卷；不要直接用仓库默认 8080 配置覆盖现有部署。后续更新镜像时同步此部署文件中的镜像标签。

一致性备份保存在被忽略的 `backups/rename-20261017/`，目录权限为 0700，快照权限为 0600。切换前后快照的 `integrity_check` 均为 `ok`，表结构及各表内容摘要相同：论文 40 行、日报任务 14 行、日报论文 40 行、推荐历史 20 行、筛选结论 96 行、人工判断 0 行、主题 Webhook 1 行。检查未输出 Webhook 或配置正文。

旧镜像保留为 `research-vault:before-rename-20261017`。若需回退，使用 `data/rename-deploy/rollback.json` 重新创建同一服务；该文件使用旧 CLI 和健康检查，但保留修正后的挂载路径、原端口及数据卷。回退不自动覆盖数据库。

执行证据与快照比较脚本保存在 `data/rename-deploy/`。本记录只证明本次更名部署的健康与数据一致性，不证明真实模型调用或群投递成功；本次验证没有调用模型或发送群消息。备份、实际配置与部署文件不提交到仓库。
