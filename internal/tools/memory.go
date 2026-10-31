package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bitvibe/kestrel/internal/store"
)

// MemoryTool manages curated notes in MEMORY.md and USER.md, mirrored into
// SQLite for search.
type MemoryTool struct {
	Store    *store.Store
	Dir      string
	MaxBytes int
	mu       sync.Mutex
}

func (t *MemoryTool) Name() string { return "memory" }
func (t *MemoryTool) Description() string {
	return "Manage persistent curated notes across sessions. Scope MEMORY is for environment/project facts, USER for stable user preferences. Actions: read, append, replace."
}
func (t *MemoryTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"action":{"type":"string","enum":["read","append","replace"]},
			"scope":{"type":"string","enum":["MEMORY","USER","all"],"description":"Default MEMORY."},
			"content":{"type":"string","description":"Text to append for action=append."},
			"old_string":{"type":"string","description":"Exact text to replace for action=replace."},
			"new_string":{"type":"string","description":"Replacement for action=replace."}
		},
		"required":["action"]
	}`)
}

func (t *MemoryTool) file(scope string) (string, string, error) {
	switch strings.ToUpper(strings.TrimSpace(scope)) {
	case "", "MEMORY":
		return "MEMORY", filepath.Join(t.Dir, "MEMORY.md"), nil
	case "USER":
		return "USER", filepath.Join(t.Dir, "USER.md"), nil
	default:
		return "", "", fmt.Errorf("scope must be MEMORY or USER")
	}
}

func (t *MemoryTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Action  string `json:"action"`
		Scope   string `json:"scope"`
		Content string `json:"content"`
		Old     string `json:"old_string"`
		New     string `json:"new_string"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	scope := strings.ToUpper(strings.TrimSpace(a.Scope))
	if scope == "" {
		scope = "MEMORY"
	}
	if scope == "ALL" {
		if a.Action != "read" {
			return ErrorResult("scope=all is only valid for read"), nil
		}
		return t.readAll(), nil
	}
	name, path, err := t.file(scope)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := os.MkdirAll(t.Dir, 0o750); err != nil {
		return ErrorResult("create memory dir: %v", err), nil
	}
	switch a.Action {
	case "read":
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return ErrorResult("read %s: %v", path, err), nil
		}
		if len(data) == 0 {
			return TextResult("(%s memory is empty)", name), nil
		}
		return Result{Content: fmt.Sprintf("# %s\n%s", name, strings.TrimSpace(string(data)))}, nil
	case "append":
		if strings.TrimSpace(a.Content) == "" {
			return ErrorResult("content is required"), nil
		}
		data, _ := os.ReadFile(path)
		if t.MaxBytes > 0 && len(data)+len(a.Content) > t.MaxBytes {
			return ErrorResult("%s memory is at its %d byte limit; consolidate it with action=replace first", name, t.MaxBytes), nil
		}
		entry := strings.TrimSpace(a.Content)
		if !strings.HasPrefix(entry, "- ") && !strings.HasPrefix(entry, "#") {
			entry = "- " + entry
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return ErrorResult("open %s: %v", path, err), nil
		}
		if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
			_, _ = f.WriteString("\n")
		}
		if _, err := f.WriteString(entry + "\n"); err != nil {
			f.Close()
			return ErrorResult("append: %v", err), nil
		}
		f.Close()
		if t.Store != nil {
			if err := t.Store.AddMemory(ctx, name, entry); err != nil {
				return ErrorResult("note saved to file but not indexed: %v", err), nil
			}
		}
		return TextResult("appended to %s memory (%d bytes total)", name, len(data)+len(entry)+2), nil
	case "replace":
		if a.Old == "" {
			return ErrorResult("old_string is required"), nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return ErrorResult("read %s: %v", path, err), nil
		}
		if !strings.Contains(string(data), a.Old) {
			return ErrorResult("old_string not found in %s memory", name), nil
		}
		out := strings.ReplaceAll(string(data), a.Old, a.New)
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			return ErrorResult("write: %v", err), nil
		}
		return TextResult("updated %s memory", name), nil
	default:
		return ErrorResult("action must be read, append or replace"), nil
	}
}

func (t *MemoryTool) readAll() Result {
	var b strings.Builder
	for _, scope := range []string{"MEMORY", "USER"} {
		_, path, _ := t.file(scope)
		data, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(&b, "# %s\n(error: %v)\n\n", scope, err)
			continue
		}
		if len(data) == 0 {
			fmt.Fprintf(&b, "# %s\n(empty)\n\n", scope)
			continue
		}
		fmt.Fprintf(&b, "# %s\n%s\n\n", scope, strings.TrimSpace(string(data)))
	}
	return Result{Content: strings.TrimSpace(b.String())}
}

// ReadMemory returns the current memory files for the system prompt.
func ReadMemory(dir string) string {
	var b strings.Builder
	for _, name := range []string{"MEMORY.md", "USER.md"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}
		if len(content) > 12000 {
			content = content[:12000] + "\n... [truncated]"
		}
		fmt.Fprintf(&b, "### %s\n%s\n\n", name, content)
	}
	return strings.TrimSpace(b.String())
}
