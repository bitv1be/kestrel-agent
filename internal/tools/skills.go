package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// SkillInfo describes a markdown procedure.
type SkillInfo struct {
	Name        string
	Description string
	Path        string
}

var skillNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// SkillsTool lists, loads, manages and installs markdown skills.
type SkillsTool struct {
	Dir  string
	Deps Deps
	mu   sync.Mutex
}

func (t *SkillsTool) Name() string { return "skills" }
func (t *SkillsTool) Description() string {
	return "List, load, create, update, search and install procedural skills stored as markdown files. Actions: list, load, create, update, search (find skills in public marketplaces), install (local path, archive, GitHub repository or direct URL). Load a matching skill when its description matches the task, then follow its instructions."
}
func (t *SkillsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["list","load","create","update","search","install"]},
			"name":{"type":"string","description":"Skill name (letters, digits, dots, dashes)."},
			"description":{"type":"string","description":"One-line description for create/update."},
			"content":{"type":"string","description":"Markdown body for create/update."},
			"query":{"type":"string","description":"Search terms for action=search."},
			"url":{"type":"string","description":"Install source: GitHub owner/repo, GitHub/skills.sh URL, or direct archive URL."},
			"path":{"type":"string","description":"Install source: local skill directory, .zip/.tar.gz/.tar archive or .md file."},
			"skill":{"type":"string","description":"Selector when a repository or archive contains several skills."},
			"force":{"type":"boolean","description":"Overwrite an existing skill with the same name."}
		},
		"required":["action"]
	}`)
}

func (t *SkillsTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Action      string `json:"action"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
		Query       string `json:"query"`
		URL         string `json:"url"`
		Path        string `json:"path"`
		Skill       string `json:"skill"`
		Force       bool   `json:"force"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	switch a.Action {
	case "list":
		skills, err := ListSkills(t.Dir)
		if err != nil {
			return ErrorResult("list skills: %v", err), nil
		}
		if len(skills) == 0 {
			return TextResult("no skills installed"), nil
		}
		var b strings.Builder
		for _, s := range skills {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		}
		return Result{Content: strings.TrimSpace(b.String())}, nil
	case "load":
		path, err := t.skillPath(a.Name)
		if err != nil {
			return ErrorResult("%v", err), nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return ErrorResult("skill %q not found", a.Name), nil
		}
		return Result{Content: string(data)}, nil
	case "create", "update":
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.createOrUpdate(a.Action, a.Name, a.Description, a.Content)
	case "search":
		return t.search(ctx, a.Query)
	case "install":
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.install(ctx, installRequest{URL: a.URL, Path: a.Path, Skill: a.Skill, Force: a.Force})
	default:
		return ErrorResult("action must be list, load, create, update, search or install"), nil
	}
}

func (t *SkillsTool) createOrUpdate(action, name, description, content string) (Result, error) {
	if !skillNameRE.MatchString(name) {
		return ErrorResult("invalid skill name (use letters, digits, dots, dashes)"), nil
	}
	if strings.TrimSpace(description) == "" {
		return ErrorResult("description is required"), nil
	}
	path, err := t.skillPath(name)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	if action == "create" {
		if _, err := os.Stat(path); err == nil {
			return ErrorResult("skill %q already exists; use action=update", name), nil
		}
	}
	if len(content) > 256*1024 {
		return ErrorResult("skill content is too large (max 256 KiB)"), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return ErrorResult("create skill dir: %v", err), nil
	}
	doc := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", name, strings.TrimSpace(description), strings.TrimSpace(content))
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		return ErrorResult("write skill: %v", err), nil
	}
	return TextResult("%s skill %q at %s", action, name, path), nil
}

func (t *SkillsTool) skillPath(name string) (string, error) {
	if !skillNameRE.MatchString(name) {
		return "", fmt.Errorf("invalid skill name %q", name)
	}
	dir, err := filepath.Abs(t.Dir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name, "SKILL.md")
	if !Within(dir, path) {
		return "", fmt.Errorf("skill path escapes the skills directory")
	}
	return path, nil
}

// ListSkills scans the skills directory.
func ListSkills(dir string) ([]SkillInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SkillInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		info, err := parseSkillFile(path)
		if err != nil {
			continue
		}
		if info.Name == "" {
			info.Name = e.Name()
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func parseSkillFile(path string) (SkillInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SkillInfo{}, err
	}
	info := SkillInfo{Path: path}
	text := string(data)
	if strings.HasPrefix(text, "---") {
		rest := text[3:]
		if i := strings.Index(rest, "\n---"); i >= 0 {
			front := rest[:i]
			for _, line := range strings.Split(front, "\n") {
				k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
				if !ok {
					continue
				}
				switch strings.ToLower(strings.TrimSpace(k)) {
				case "name":
					info.Name = strings.Trim(strings.TrimSpace(v), `"'`)
				case "description":
					info.Description = strings.Trim(strings.TrimSpace(v), `"'`)
				}
			}
		}
	}
	if info.Description == "" {
		body := text
		if i := strings.Index(body, "\n---"); i >= 0 {
			body = body[i+4:]
		}
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				info.Description = line
				break
			}
		}
	}
	if len(info.Description) > 300 {
		info.Description = info.Description[:300]
	}
	return info, nil
}
