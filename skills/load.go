// Package skills loads the Agent Skills that drive model screening and summaries.
// Defaults are embedded in the binary. A configured directory overrides a skill
// when that directory contains a valid SKILL.md for the same name.
package skills

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// Screening is the default fine-grained relevance skill.
	Screening = "paper-screening"
	// Summary is the default per-paper summary skill.
	Summary = "paper-summary"
)

//go:embed paper-screening/SKILL.md paper-summary/SKILL.md
var embeddedFS embed.FS

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Skill is one Agent Skill: YAML front matter plus the markdown instructions.
type Skill struct {
	Name        string
	Description string
	Body        string
	Path        string
}

// Version identifies the instruction text. Editing the body changes it.
func (s Skill) Version() string {
	return Version(s.Name, s.Body)
}

// Version is name plus a short hash of the instruction body.
func Version(name, body string) string {
	sum := sha256.Sum256([]byte(body))
	return name + "@" + hex.EncodeToString(sum[:4])
}

// Set is the embedded defaults with optional directory overrides applied.
type Set struct {
	skills map[string]Skill
}

// Get returns one skill by its front-matter name.
func (s Set) Get(name string) (Skill, error) {
	skill, ok := s.skills[name]
	if !ok {
		return Skill{}, fmt.Errorf("没有名为 %s 的技能", name)
	}
	return skill, nil
}

// Load reads the embedded skills, then overlays SKILL.md files from dir.
// An empty dir uses only the embedded copies. A configured dir must exist.
// Any SKILL.md in that dir that fails validation aborts the load; a missing
// name falls back to the embedded skill.
func Load(dir string) (Set, error) {
	embedded, err := loadEmbedded()
	if err != nil {
		return Set{}, err
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return Set{skills: embedded}, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return Set{}, fmt.Errorf("读取技能目录失败: %w", err)
	}
	if !info.IsDir() {
		return Set{}, errors.New("技能目录必须是文件夹")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Set{}, fmt.Errorf("读取技能目录失败: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return Set{}, fmt.Errorf("读取技能 %s 失败: %w", path, err)
		}
		skill, err := Parse(string(data))
		if err != nil {
			return Set{}, fmt.Errorf("技能 %s 无效: %w", path, err)
		}
		if skill.Name != entry.Name() {
			return Set{}, fmt.Errorf("技能 %s 的 name 是 %s，必须与文件夹名一致", path, skill.Name)
		}
		skill.Path = path
		embedded[skill.Name] = skill
	}
	return Set{skills: embedded}, nil
}

func loadEmbedded() (map[string]Skill, error) {
	out := make(map[string]Skill, 2)
	for _, name := range []string{Screening, Summary} {
		data, err := embeddedFS.ReadFile(name + "/SKILL.md")
		if err != nil {
			return nil, fmt.Errorf("读取内嵌技能 %s 失败: %w", name, err)
		}
		skill, err := Parse(string(data))
		if err != nil {
			return nil, fmt.Errorf("内嵌技能 %s 无效: %w", name, err)
		}
		if skill.Name != name {
			return nil, fmt.Errorf("内嵌技能 %s 的 name 不一致", name)
		}
		out[name] = skill
	}
	return out, nil
}

// Parse validates SKILL.md text. The name comes from the front matter.
func Parse(text string) (Skill, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return Skill{}, errors.New("SKILL.md 必须以 --- 开头的前言开始")
	}
	rest := strings.TrimPrefix(text, "---\n")
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return Skill{}, errors.New("SKILL.md 前言缺少结束标记 ---")
	}
	name, description, err := parseFront(rest[:end])
	if err != nil {
		return Skill{}, err
	}
	body := strings.TrimSpace(rest[end+len("\n---\n"):])
	if body == "" {
		return Skill{}, errors.New("SKILL.md 缺少说明正文")
	}
	if !namePattern.MatchString(name) {
		return Skill{}, errors.New("技能 name 只能使用小写字母、数字和连字符")
	}
	if strings.TrimSpace(description) == "" {
		return Skill{}, errors.New("技能 description 不能为空")
	}
	return Skill{Name: name, Description: strings.TrimSpace(description), Body: body}, nil
}

func parseFront(front string) (string, string, error) {
	var name, description, pending string
	var folded []string
	finish := func() {
		if pending == "description" {
			description = strings.Join(folded, " ")
			pending = ""
			folded = nil
		}
	}
	for _, line := range strings.Split(front, "\n") {
		if pending == "description" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			folded = append(folded, strings.TrimSpace(line))
			continue
		}
		finish()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return "", "", errors.New("技能前言的每一行都要是 key: value")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "name":
			name = unquote(value)
		case "description":
			if value == "" || value == "|" || value == ">" {
				pending = "description"
				continue
			}
			description = unquote(value)
		}
	}
	finish()
	if name == "" {
		return "", "", errors.New("技能前言缺少 name")
	}
	return name, description, nil
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
