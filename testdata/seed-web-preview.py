import argparse
import json
import sqlite3
from pathlib import Path

parser = argparse.ArgumentParser(description='创建独立的双主题虚构演示数据库。')
parser.add_argument('--output', default='data/web-preview', help='相对于仓库根目录的输出目录')
args = parser.parse_args()
root = Path(__file__).resolve().parents[1] / args.output
root.mkdir(parents=True, exist_ok=True)
if (root / 'digest.db').exists():
    raise SystemExit('演示数据库已存在；为避免覆盖，请用 --output 指定新的目录。')
conn = sqlite3.connect(root / 'digest.db')
conn.executescript('''
CREATE TABLE IF NOT EXISTS jobs (topic TEXT NOT NULL,date TEXT NOT NULL,status TEXT NOT NULL,message TEXT NOT NULL DEFAULT '',PRIMARY KEY(topic,date));
CREATE TABLE IF NOT EXISTS papers (topic TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(topic,paper_id,version));
CREATE TABLE IF NOT EXISTS job_papers (topic TEXT NOT NULL,date TEXT NOT NULL,paper_id TEXT NOT NULL,version TEXT NOT NULL,position INTEGER NOT NULL,summary BLOB,PRIMARY KEY(topic,date,paper_id));
CREATE TABLE IF NOT EXISTS recommendations (topic TEXT NOT NULL,paper_id TEXT NOT NULL,date TEXT NOT NULL,version TEXT NOT NULL,PRIMARY KEY(topic,paper_id));
''')
topic = 'recommendation-advertising-search'
entries = [
    ('2610.00001', 'A Unified Retrieval Framework for Recommendation and Search', ['Lin Chen', 'Alex Morgan', 'Wei Zhang'], '检索框架（演示数据）', '• 研究问题：推荐与搜索常使用独立的检索模型，难以共享用户意图。\n\n• 方法：提出统一的双塔检索框架，对查询和用户历史采用共同的表示空间，评分为 $s(u,i)=\\mathbf{u}^{T}\\mathbf{v}_i$。\n\n$$p(i \\mid u)=\\frac{e^{s(u,i)}}{\\sum_j e^{s(u,j)}}$$\n\n• 结果：公开摘要报告了离线召回改进，但未给出全部实验细节。\n\n• 局限：以上内容为页面演示数据，不代表真实论文结论。'),
    ('2610.00002', 'Learning Long-Term User Preferences from Sparse Implicit Feedback', ['Maya Patel', 'Jun Liu'], '长期偏好（演示数据）', None),
    ('2610.00003', 'Counterfactual Evaluation of Advertising Policies under Distribution Shift', ['Rui Wang', 'Sofia Garcia', 'Noah Kim'], '广告评估（演示数据）', '• 使用反事实方法评估广告策略。\n\n• 关注分布变化下的估计稳定性。\n\n• 本条目仅用于界面演示。'),
]
for date, status in [('2026-10-01', 'ready'), ('2026-09-30', 'unknown')]:
    conn.execute('INSERT OR REPLACE INTO jobs VALUES(?,?,?,?)', (topic,date,status,'【虚构演示日报】\n以上标题、作者和摘要仅供页面验收，不是真实论文。'))
    for position, (identifier,title,authors,abstract,summary) in enumerate(entries):
        version = 'v2' if date == '2026-10-01' else 'v1'
        stable_id = 'arxiv:' + identifier
        paper = dict(ID=stable_id,Version=version,Title=title + ' [Demo]',Authors=authors,Published='2026-09-29T08:00:00Z',Updated='2026-09-30T08:00:00Z',Abstract=abstract+'\nThis fictional paper is provided for interface verification only. It describes a research problem, a proposed method, and limitations based on an abstract. No claims refer to a real publication.',URL='https://arxiv.org/abs/'+identifier+version)
        analysis = json.dumps(dict(Text=summary,Model='demo-model',PromptVersion='demo')) if summary else None
        conn.execute('INSERT OR REPLACE INTO papers VALUES(?,?,?,?)', (topic,stable_id,version,json.dumps(paper)))
        conn.execute('INSERT OR REPLACE INTO job_papers VALUES(?,?,?,?,?,?)', (topic,date,stable_id,version,position,analysis))
conn.execute('INSERT OR REPLACE INTO jobs VALUES(?,?,?,?)', (topic,'2026-09-29','missed',''))
other_topic = 'demo-other-topic'
conn.execute('INSERT INTO jobs VALUES(?,?,?,?)', (other_topic, '2026-10-01', 'ready', '【另一个主题的虚构演示日报】\n用于验证相同日期与论文 ID 不会串到其他主题。'))
other_paper = dict(ID='arxiv:2610.00001', Version='v1', Title='Independent Topic Paper [Demo]', Authors=['Demo Author'], Published='2026-09-29T08:00:00Z', Updated='2026-09-30T08:00:00Z', Abstract='另一个主题的独立摘要，仅用于主题隔离验收。', URL='https://arxiv.org/abs/2610.00001v1')
conn.execute('INSERT INTO papers VALUES(?,?,?,?)', (other_topic, other_paper['ID'], other_paper['Version'], json.dumps(other_paper)))
conn.execute('INSERT INTO job_papers VALUES(?,?,?,?,?,?)', (other_topic, '2026-10-01', other_paper['ID'], other_paper['Version'], 0, json.dumps(dict(Text='另一个主题的中文要点 [Demo]', Model='demo-model', PromptVersion='demo'))))
conn.commit()
conn.close()
(root / 'config.json').write_text(json.dumps(dict(database=dict(path=str(root / 'digest.db')),delivery=dict(enabled=False),anthropic=dict(api_key='',model='',base_url=''),arxiv=dict(lookback_days=7),topics=[dict(id=topic),dict(id=other_topic)]), indent=2))
