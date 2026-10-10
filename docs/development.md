# 开发指南

[文档导航](README.md) · [配置](configuration.md) · [运行与目录职责](architecture.md)

## 环境与构建

从仓库根目录执行。Go 版本由 `go.mod` 指定为 1.27，Dockerfile 使用 Node 24；依赖分别锁定在 `go.sum` 和 `web/package-lock.json`。宿主机需要 Go 1.27、Node 24、npm 和可选的 Make；演示数据脚本使用 Python 3 标准库。

```bash
npm --prefix web ci
make check
make build
```

`make check` 依次执行 `go test ./...`、`go vet ./...`、`npm --prefix web run build`；前端构建包含 TypeScript 检查。`make build` 同时生成 `web/dist/` 和 `bin/paper-digest`。Go 服务从磁盘读取前端，资源没有嵌入二进制，因此发布时两者都需要。

没有 Make 时直接执行：

```bash
go test ./...
go vet ./...
npm --prefix web run build
mkdir -p bin
go build -o bin/paper-digest ./cmd/paper-digest
```

## 宿主机运行

Compose 示例的 `/data/digest.db` 属于容器；宿主机使用独立配置和本地目录：

```bash
cp config/config.local.example.json config/config.local.json
chmod 600 config/config.local.json
mkdir -p data/local
npm --prefix web run build
go run ./cmd/paper-digest --config config/config.local.json serve
```

打开 http://127.0.0.1:8080。该示例关闭自动任务；无需凭据，数据库为空。实际配置被忽略，不应提交。`serve` 默认监听 `127.0.0.1:8080` 并读取 `web/dist`，可用 `--listen`、`--web-dir` 修改。`--config` 必须位于子命令之前；相对配置、数据库和静态资源路径均基于进程工作目录。

## 前后端双终端开发

先完成上述配置和前端首次构建。终端一运行：

```bash
make dev
# 等价于 go run ./cmd/paper-digest --config config/config.local.json serve
```

终端二运行：

```bash
make web-dev
# 等价于 npm --prefix web run dev
```

打开 http://127.0.0.1:5173；Vite 将 `/api` 代理到 `http://127.0.0.1:8080`。修改后端监听端口时同步修改 `web/vite.config.ts` 的代理地址。5173 被占用时 Vite 会失败，不会自动换端口。前端热更新来自 Vite，Go 托管的 `web/dist` 需重新构建才会更新。

## 离线界面演示

```bash
npm --prefix web ci
npm --prefix web run build
python3 testdata/seed-web-preview.py
go run ./cmd/paper-digest --config data/web-preview/config.json serve
```

打开 http://127.0.0.1:8080。脚本只写入被忽略的 `data/web-preview/`，创建两个主题，使用相同日期和论文 ID 的不同虚构内容验证隔离，标题均标记 `[Demo]`。不会访问 arXiv、模型或飞书，实际论文库也不会自动加载这些数据。

已有演示数据库时脚本拒绝覆盖；可创建新的隔离目录：

```bash
python3 testdata/seed-web-preview.py --output data/web-preview-2
go run ./cmd/paper-digest --config data/web-preview-2/config.json serve
```

## CLI 与验证范围

| 命令 | 行为 |
| --- | --- |
| `serve [--listen <地址>] [--web-dir <目录>]` | 页面 + API；仅在 `delivery.enabled=true` 时启动 worker |
| `health [--url <地址>]` | 检查正在运行的 HTTP 与数据库；不读取配置 |
| `status [YYYY-MM-DD]` | 查询联合主题任务；默认北京时间当天，无任务时失败 |
| `preview <fixture.json>` | 离线渲染 Markdown 到标准输出；不读配置或数据库 |
| `eval --date YYYY-MM-DD [--to YYYY-MM-DD] [--input papers.json] [--json]` | 用当前规则预览选题；不调用模型、不发送、不写日报或去重记录 |
| `eval label --id <id> --label relevant\|not-relevant\|clear [--input papers.json]` | 保存或清除一篇论文的人工判断 |
| `eval fixtures` | 把已标注且有论文快照的记录导出为回归样本 |
| `backup <未存在的文件>` | 打开配置中的数据库并生成一致性快照 |
| `send-test --topic <id> --confirm` | 一条真实测试群消息；先遵循 [运维试发流程](operations.md#受控飞书试发) |

```bash
make preview
# 服务运行后，另一个终端检查健康
go run ./cmd/paper-digest health
```

Go 测试以 fixture、临时 SQLite 和模拟 HTTP 为主，不验证真实投递或真实模型可用性。仓库当前没有 CI 工作流，提交前需手动运行 `make check`。改变运行语义、配置字段、API 或文件路径时，同步更新对应文档与引用。
