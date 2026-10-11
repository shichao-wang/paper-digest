package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedSkillsMatchTheirFolders(t *testing.T) {
	set, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{Screening, Summary} {
		skill, err := set.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if skill.Name != name || skill.Description == "" || skill.Body == "" || skill.Path != "" {
			t.Fatalf("%s = %+v", name, skill)
		}
		if !strings.HasPrefix(skill.Version(), name+"@") || len(skill.Version()) != len(name)+1+8 {
			t.Fatalf("version = %s", skill.Version())
		}
	}
	summary, err := set.Get(Summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary.Body, "只能依据") || !strings.Contains(summary.Body, "- **标签**：正文") {
		t.Fatalf("summary skill lost the card instructions: %s", summary.Body)
	}
	screening, err := set.Get(Screening)
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{
		"下面几类之一时判为相关",
		"推荐与检索模型的核心训练技术",
		"sampled softmax",
		"修正两级 softmax 采样偏差的方法",
		"这几类里，分数越高",
		"上面「收」中列出的训练技术不属于此类",
		`{"decisions":[{"id":"arxiv:0000.00000","relevant":false,"score":0.1,"reason":"一句中文理由"}]}`,
	} {
		if !strings.Contains(screening.Body, phrase) {
			t.Fatalf("screening skill missing %q", phrase)
		}
	}
	if strings.Contains(screening.Body, "三类之一") || strings.Contains(screening.Body, "softmax 采样、聚类采样") {
		t.Fatalf("screening skill still excludes softmax sampling as generic ML: %s", screening.Body)
	}
}

func TestDirectoryOverrideReplacesOneSkillAndRejectsInvalidFiles(t *testing.T) {
	set, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	original, err := set.Get(Screening)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeSkill(t, dir, Screening, "---\nname: "+Screening+"\ndescription: 草稿精筛\n---\n只保留检索论文。\n")
	loaded, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := loaded.Get(Screening)
	if err != nil || !strings.Contains(replaced.Body, "只保留检索论文") || replaced.Path == "" {
		t.Fatalf("override = %+v, err %v", replaced, err)
	}
	summary, err := loaded.Get(Summary)
	if err != nil || summary.Body != mustSummary(t, original) {
		t.Fatal("missing override should keep the embedded summary")
	}
	if summary.Body == "" {
		t.Fatal("embedded summary body is empty")
	}

	bad := t.TempDir()
	writeSkill(t, bad, "custom-screen", "---\nname: other-name\ndescription: 不一致\n---\n正文\n")
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "文件夹名") {
		t.Fatalf("name mismatch error = %v", err)
	}
	empty := t.TempDir()
	writeSkill(t, empty, "broken-skill", "没有前言\n")
	if _, err := Load(empty); err == nil {
		t.Fatal("invalid SKILL.md was accepted")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing skills dir was accepted")
	}
}

func TestInstructionKeepsActiveMetadataForBodyOnlyText(t *testing.T) {
	fallback := Skill{Name: Screening, Description: "现行精筛", Body: "旧正文"}
	body := "只保留检索论文。"
	skill, err := Instruction(body, fallback)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Name != fallback.Name || skill.Description != fallback.Description || skill.Body != body {
		t.Fatalf("body-only = %+v", skill)
	}
	if skill.Version() != Version(Screening, body) {
		t.Fatalf("version = %s", skill.Version())
	}
	full := "---\nname: draft-screen\ndescription: 草稿精筛\n---\n完整判断。\n"
	parsed, err := Instruction(full, fallback)
	if err != nil || parsed.Name != "draft-screen" || parsed.Body != "完整判断。" || parsed.Description != "草稿精筛" {
		t.Fatalf("full document = %+v, %v", parsed, err)
	}
	if _, err := Instruction("---\nname: broken", fallback); err == nil {
		t.Fatal("incomplete front matter was accepted as a body")
	}
	if _, err := Instruction("   ", fallback); err == nil {
		t.Fatal("empty instruction was accepted")
	}
}

func TestParseRejectsIncompleteSkills(t *testing.T) {
	for _, text := range []string{
		"",
		"---\nname: paper-screening\n---\n正文\n",
		"---\nname: paper-screening\ndescription:\n---\n\n",
		"---\nname: Paper\ndescription: 说明\n---\n正文\n",
	} {
		if _, err := Parse(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	skill, err := Parse("---\nname: draft-screen\ndescription: >\n  折叠说明\n---\n正文判断。\n")
	if err != nil || skill.Description != "折叠说明" || skill.Body != "正文判断。" {
		t.Fatalf("folded = %+v, %v", skill, err)
	}
}

func writeSkill(t *testing.T, dir, name, text string) {
	t.Helper()
	folder := filepath.Join(dir, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "SKILL.md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSummary(t *testing.T, _ Skill) string {
	t.Helper()
	set, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	skill, err := set.Get(Summary)
	if err != nil {
		t.Fatal(err)
	}
	return skill.Body
}
