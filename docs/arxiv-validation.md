# arXiv 来源验证（计划第一步）

验证日期：2026-10-02（北京时间）。实际抓取窗口：2026-10-01 17:03:40–17:04:52 UTC，即北京时间 2026-10-02 01:03:40–01:04:52。**被验证的公告批次为 2026-10-01，不能用本机日期替代公告日期。**

结论：**第一步中五分类当前批次来源与完整性验证通过；错过批次的历史修订补扫未证实，仍有 gap。** 本次只验证公开来源、保存样例和可重跑脚本，不实施采集流水线、不读取密钥、不运行模型、不提交 Git。

## 实测通过

使用官方 `https://rss.arxiv.org/atom/{category}`，按 cs.IR、cs.LG、cs.AI、cs.CL、stat.ML 顺序抓取，再抓对应 `https://arxiv.org/list/{category}/new?show=1000`。十个响应均为 HTTP 200，Atom XML 完整解析。列表均明确写出 `Showing new listings for Thursday, 1 October 2026`、`Total of N entries`，且三个分组 `showing X of X entries` 全部显示。不是抽样或首页比较。

| 分类 | new | cross | replace | replace-cross | Atom 项数 | 官方 new 列表明确总数 | 列表 replacement 总数 | 稳定 ID 集合差异 |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| cs.IR | 16 | 13 | 8 | 4 | 41 | 41 | 12 | 双向均为空 |
| cs.LG | 256 | 172 | 133 | 77 | 638 | 638 | 210 | 双向均为空 |
| cs.AI | 133 | 261 | 72 | 147 | 613 | 613 | 219 | 双向均为空 |
| cs.CL | 115 | 64 | 50 | 40 | 269 | 269 | 90 | 双向均为空 |
| stat.ML | 23 | 31 | 11 | 16 | 81 | 81 | 27 | 双向均为空 |

逐分类检查了 Atom 的 entry 数量、稳定 ID 唯一性、所有 entry 的 published 日期、HTML 公告日期、HTML 明确总数及分组显示总数。分别将 Atom new、cross、replace+replace-cross 的 ID 集合与 HTML New submissions、Cross submissions、Replacement submissions 比对，三个分组也完全相等。

由此可以确认**本次捕获的同一个批次**在这五个单分类 feed 中完整。不能据此保证未来任何批次完整。若后续实施，每批应保存原文证据，并与官方 new 页同批总数、分组数量及 ID 全集核对；跨午夜或不同缓存批次时应视为未验证，不能拿两个日期的数据直接判漏。HTML 的显示量不足、缺少总数、Atom 空但列表非空、日期不一致均不能宣告成功。当前 `show=1000` 足够覆盖本批最大 638 项；没有验证更大的请求或未来超过这个显示量的批次。

### 字段语义与真实 vN

| 字段 | 本次实际内容 | 可采用的解释与边界 |
|---|---|---|
| Atom feed id | `http://rss.arxiv.org/atom/cs.IR` 等 | feed 身份，不能当论文 ID |
| Atom entry id | `oai:arXiv.org:2609.38353v1` | 解析 stable ID `2609.38353` 与版本 `1`；本批实际出现 v1 至 v5 |
| alternate link | `https://arxiv.org/abs/2609.38353` | 这里没有 vN；不能仅从此链接推断版本 |
| `arxiv:announce_type` | new / cross / replace / replace-cross | 是相对当前 feed 分类的公告事件；同一版本在其他分类的类型可以不同 |
| category | 多个 `term`，例如 cs.IR、cs.AI | 保留完整分类集合；本次 new/replace 第一项与本分类一致，cross/replace-cross 第一项为其他主分类；不应把所属类别都当作独立论文 |
| summary | `arXiv:…vN Announce Type: … Abstract: …` | 包含公开摘要与公告类型前缀，不是纯摘要；结构化类型优先读扩展字段 |
| published | 五分类所有 entry 都是 `2026-10-01T00:00:00-04:00` | 本次 RSS Atom 的公告日时间，不是论文首次提交时间；旧稿 cross/修订也完全一样 |
| feed/entry updated | 当天约 `04:00:xx+00:00`；stat.ML 约 `04:11:18+00:00` | 本次表现为 feed 生成时间及逐 entry 时间，不能当论文修订提交时间 |
| dc:creator / dc:rights | 公开作者列表 / 许可 URL | 来源使用 Dublin Core 扩展，本次没有 API 常见的逐 author/name 结构；不能照搬 API Atom 解析假设 |

cs.IR 的四类公开真实样例（已保存 unchanged entry 摘录）：

- new：`2609.38353v1`，主分类 cs.IR，亦归 cs.AI；cs.AI 中是同一版本的 cross。
- cross：`2602.19001v2`，主分类 cs.CV，亦归 cs.AI/cs.CL/cs.IR。**cross 不等于 v1，也不等于首次发表的新论文。**
- replace：`2603.02561v2`，主分类 cs.IR，亦归 cs.CV/cs.LG。
- replace-cross：`2605.29307v2`，主分类 cs.CL，亦归 cs.AI/cs.IR/cs.LG。

五 feed 共 1,642 个分类公告项，按 `(stable ID, version)` 去重后 1,155 个论文版本，重复分类项 487 个。本批未见同一 stable ID 的不同版本同时出现；这不证明以后不会出现。fixture 用 `2609.38353v1` 在 cs.IR 与 cs.AI 的两个真实 entry 检查去重，两条记录的 type 不同、updated 也不同，因此应保留分类公告信息，不能要求这些字段完全一致才合并。论文实体键可用 stable ID，版本记录键必须保留 vN；批次事件还需公告日期与分类。

### 多分类 2000 上限

已抓取的[官方 RSS 文档](https://info.arxiv.org/help/rss.html)在“Subscribe by multiple categories”下明确写明：`Request multiple categories by appending a plus sign ... Limit 2000 results.`，同时给出 RSS 与 Atom 多分类 URL。

这是**官方文档证实的上限**，不是本次压测证实：没有构造巨大联合查询，没有探测 2001 条、截断排序或分页。文档没有给出任意未来单分类批次永远完整的保证。本次单分类的完整性来自 HTML 全集比对，不能把它升级成未来保证。

## 历史补扫：部分实测通过，完整修订恢复未证实

只以 cs.IR 做有限历史探索，各 URL 一次请求：

| 官方入口 | 结果 | 本次能证实的范围 |
|---|---|---|
| `/list/cs.IR/recent?show=250` | 200，明确共 162 项，完整显示 | 2026-10-01、09-30、09-29、09-28、09-25 五个公告日期，分别 29/29/55/19/30 项 |
| `/list/cs.IR/pastweek?show=250` | 200，同样共 162 项与上述日组 | 日组与稳定 ID 集合相同；导航使用 recent 的 skip 链接 |
| `/list/cs.IR/2026-10-01?show=1000` | **400 Bad Request** | 此 ISO 日期路径尝试失败；不能据此断言一切未知日期端点都不存在 |

recent/pastweek 的 2026-10-01 日组 29 个稳定 ID **精确等于**本批 cs.IR 的 new 16 + cross 13；当天 12 个 replace/replace-cross 不在当天日组中。页面里部分跨类项明确带 `(cross-list from …)`，可区分新稿列表中的 cross，但这些历史列表没有完整四类公告事件或版本号字段。

两个今天修订的论文 `2609.37183`、`2609.37574` 在 recent 的 **09-30 日组**出现。它们是在历史新稿/跨类列表里的论文身份，并非今天 revision 事件的恢复。这说明“历史页里找到某个旧稿 ID”不能作为“成功补扫该稿今天修订”的证据。

因此 recent/pastweek 可以提供本次窗口内 new/cross 的候选恢复入口；只实测了 cs.IR，没有声称五分类历史全量均可恢复。错过的 replace/replace-cross、准确历史 vN、任意日期完整公告批次仍未找到经验证可用的恢复入口，必须记为 **gap**。没有用首次发表月份列表替代按公告日的事件恢复：`/list/{category}/YYMM` 即使可列出该月首次发表论文，也不等价于该月 old-paper replacement 公告集合；本次未抓月份大列表，不把它作为已验证历史方案。

### 与 arXiv 查询 API 时间语义的区别

已抓取[官方 API User Manual](https://info.arxiv.org/help/api/user-manual.html)，文档明确：API entry published 是 v1 提交/处理时间，updated 是取回版本的提交/处理时间；v1 时二者相等，v2+ 可不同。这个定义不能移植到 RSS Atom feed：本次旧稿 `2602.19001v2`、`2603.02561v2` 的 RSS published 仍为公告日 2026-10-01。

手册给出 `submittedDate` 查询范围以及 `lastUpdatedDate` 排序参数；**没有证实 submittedDate 能按今天的修订日期返回首次提交在过去的旧稿**。更不能把按 submittedDate 的近 N 天查询当成 replacement 历史恢复。本次没有向 export API 执行查询，没有验证 lastUpdatedDate 可恢复完整修订事件日志；排序参数和已知 ID 最新元数据都不等于历史公告批次档案。这部分保持未证实。

## 失败、未证实与实现前的边界

- **失败**：ISO 日期形式 list 探测 HTTP 400；原错误响应与时间、sha256 已保存。其余 14 请求成功，未发生网络受限、超时或绕过。
- **未证实**：未来单分类完整性、真实空批次、超过限额截断行为/排序、多分类超过 2000、所有分类 historical new/cross 恢复、历史 replace/replace-cross 与 vN 恢复、API 修订补扫。空/截断 fixture 都是明确标注的合成数据，仅验证解析边界。
- **需要保留的 gap**：日批次未及时保存时，当前证据不足以宣称能完整恢复修订公告。后续流水线方案不得以近 N 天 submittedDate 或首次发表月份列表掩盖此缺口。
- **实现建议而非本次实现**：每批同日期的官方 new 全量核对、缺失批次标记、按 ID+版本合并同时保留类别事件、RSS/API 时间字段分别处理。本次没有修改生产抓取或数据库。

## 证据保存与重跑

本地原始全量（原先 `.gitignore` 已忽略整个 data/，无需改忽略规则）：

- `data/validation/arxiv/manifest.json`：15 个 URL、requested_at/finished_at、状态、最终 URL、响应 headers、字节数、sha256。
- `data/validation/arxiv/atom-*.xml`、`new-*.html`、`recent-cs.IR.html`、`pastweek-cs.IR.html`、`dated-cs.IR.error.txt`、两份官方说明 HTML：原响应字节，未改写。
- `data/validation/arxiv/analysis.json`：完整解析出的 ID/版本/字段、分组及全集比较、跨分类版本合并统计。原文 sha256 在每次分析时核对。

可进入版本管理的精简公开 fixture：

- `testdata/validation/arxiv/real-entries.xml`：五条来自 cs.IR/cs.AI 的真实公开 entry，覆盖四种类型与跨分类重复。entry 内容原样保留，聚合 wrapper 明确标注为样例，不能作为完整官方 feed。
- `testdata/validation/arxiv/real-cs.IR-list-extract.html`：官方公告日期、总数、分组标题与 41 个 item header 的精简结构摘录；移除摘要、作者、导航，明确不是完整原文。
- `synthetic-empty.xml` / `synthetic-truncated.xml`：分别是合成空 feed 和故意截断 XML；不是实际观察到的 arXiv 空批次或网络失败。
- `index.json`：fixture 身份、sha256、大小、真实样例来源、全部原文 URL/抓取时间/sha256 和此次统计。不含 Cookie、凭证或非公开资料。原响应全量仅保存在本机 gitignored 目录；换机器重跑需重新下载或由用户自行保留该目录。

标准库脚本 `scripts/verify-arxiv.py` 默认只离线分析缓存；下载模式 20 秒 socket timeout、顺序请求、请求开始至少间隔 3 秒、不重试、不并发；单轮最多 15 个固定 URL，约 330 秒下载预算。网络拒绝或超时直接记错误，不改协议/代理/镜像绕过。总预算为停止发起后续请求的软边界；底层 socket timeout 不是整次 HTTP 流式传输的严格墙钟硬截止。此次完整网络窗口 71.4 秒，最大原文约 2 MB，未触及 8 MiB 捕获保护。

只读取本地 raw cache，核对 sha256 并重算完整性：

```bash
python3 scripts/verify-arxiv.py
```

只有精简公开 fixture 也可重跑，不需网络：

```bash
python3 scripts/verify-arxiv.py --self-check
```

另一个干净目录单轮重抓；已存在 manifest 的目录拒绝隐式重复下载：

```bash
python3 scripts/verify-arxiv.py --download --data-dir data/validation/arxiv-next --history-date 2026-10-01
```

验证结果：离线分析五分类 comparisons 全部 `passed`；fixture self-check 通过四类型、版本识别、跨分类 ID+版本去重、合成空 XML、合成截断拒绝、HTML 日期与明确分组总数检查。`--self-check` 校验的是当前固定真实样例，未来另一个日期的采集需使用离线 analysis 输出，不会把合成空或旧样例用作当前批次证据。
