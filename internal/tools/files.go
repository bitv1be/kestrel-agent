package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ReadFileTool reads a text file from an allowed root.
type ReadFileTool struct{ Roots []string }

func (t *ReadFileTool) Name() string { return "read_file" }
func (t *ReadFileTool) Description() string {
	return "Read a UTF-8 text file inside the allowed workspace roots. Use offset/limit for large files. Reads are capped to protect memory."
}
func (t *ReadFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"File path, absolute or relative to the first root."},
			"offset":{"type":"integer","description":"1-based line to start at."},
			"limit":{"type":"integer","description":"Maximum number of lines (default 2000)."}
		},
		"required":["path"]
	}`)
}

func (t *ReadFileTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	p, err := SecurePath(t.Roots, a.Path)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	data, truncated, err := readLimited(p, 512*1024)
	if err != nil {
		return ErrorResult("read %s: %v", p, err), nil
	}
	text := string(data)
	lines := strings.Split(text, "\n")
	start := a.Offset
	if start > 1 {
		if start > len(lines) {
			return TextResult("(offset beyond end of file; %d lines)", len(lines)), nil
		}
		lines = lines[start-1:]
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 2000
	}
	if limit < len(lines) {
		lines = lines[:limit]
		truncated = true
	}
	var b strings.Builder
	b.WriteString(strings.Join(lines, "\n"))
	if truncated {
		b.WriteString("\n\n... [truncated; read with offset/limit for the rest] ...")
	}
	return Result{Content: b.String()}, nil
}

// WriteFileTool writes or appends text files inside the roots.
type WriteFileTool struct {
	Roots   []string
	Enabled bool
}

func (t *WriteFileTool) Name() string { return "write_file" }
func (t *WriteFileTool) Description() string {
	return "Create or overwrite a text file inside the allowed workspace roots. Parent directories are created."
}
func (t *WriteFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string"},
			"content":{"type":"string"},
			"append":{"type":"boolean","description":"Append instead of overwriting."}
		},
		"required":["path","content"]
	}`)
}

func (t *WriteFileTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	if !t.Enabled {
		return ErrorResult("file writes are disabled by configuration"), nil
	}
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Append  bool   `json:"append"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	if len(a.Content) > 8*1024*1024 {
		return ErrorResult("content is too large (max 8 MiB)"), nil
	}
	p, err := SecurePath(t.Roots, a.Path)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return ErrorResult("create parent directory: %v", err), nil
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if a.Append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(p, flags, 0o644)
	if err != nil {
		return ErrorResult("write %s: %v", p, err), nil
	}
	if _, err := f.WriteString(a.Content); err != nil {
		f.Close()
		return ErrorResult("write %s: %v", p, err), nil
	}
	if err := f.Close(); err != nil {
		return ErrorResult("close %s: %v", p, err), nil
	}
	if a.Append {
		return TextResult("appended %d bytes to %s", len(a.Content), p), nil
	}
	return TextResult("wrote %d bytes to %s", len(a.Content), p), nil
}

// EditFileTool replaces exact text in a file.
type EditFileTool struct {
	Roots   []string
	Enabled bool
}

func (t *EditFileTool) Name() string { return "edit_file" }
func (t *EditFileTool) Description() string {
	return "Replace an exact substring in a text file. By default old_string must occur exactly once; set replace_all to change every occurrence."
}
func (t *EditFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string"},
			"old_string":{"type":"string","description":"Exact text to replace."},
			"new_string":{"type":"string","description":"Replacement text."},
			"replace_all":{"type":"boolean"}
		},
		"required":["path","old_string","new_string"]
	}`)
}

func (t *EditFileTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	if !t.Enabled {
		return ErrorResult("file writes are disabled by configuration"), nil
	}
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	if a.Old == "" {
		return ErrorResult("old_string must not be empty"), nil
	}
	p, err := SecurePath(t.Roots, a.Path)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ErrorResult("read %s: %v", p, err), nil
	}
	count := strings.Count(string(data), a.Old)
	if count == 0 {
		return ErrorResult("old_string was not found in %s", p), nil
	}
	if count > 1 && !a.ReplaceAll {
		return ErrorResult("old_string occurs %d times; add context to make it unique or set replace_all", count), nil
	}
	var out string
	if a.ReplaceAll {
		out = strings.ReplaceAll(string(data), a.Old, a.New)
	} else {
		out = strings.Replace(string(data), a.Old, a.New, 1)
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		return ErrorResult("write %s: %v", p, err), nil
	}
	return TextResult("replaced %d occurrence(s) in %s", count, p), nil
}

// ListDirTool lists directory entries.
type ListDirTool struct{ Roots []string }

func (t *ListDirTool) Name() string { return "list_dir" }
func (t *ListDirTool) Description() string {
	return "List files and directories under an allowed root."
}
func (t *ListDirTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"Directory, defaults to the first root."},
			"recursive":{"type":"boolean"},
			"max_entries":{"type":"integer","description":"Default 300, max 2000."}
		}
	}`)
}

func (t *ListDirTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Path       string `json:"path"`
		Recursive  bool   `json:"recursive"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	p, err := SecurePath(t.Roots, a.Path)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	max := a.MaxEntries
	if max <= 0 {
		max = 300
	}
	if max > 2000 {
		max = 2000
	}
	var b strings.Builder
	n := 0
	truncated := false
	if a.Recursive {
		err = filepath.WalkDir(p, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				fmt.Fprintf(&b, "! %s: %v\n", path, walkErr)
				return nil
			}
			if path == p {
				return nil
			}
			if n >= max {
				truncated = true
				return filepath.SkipAll
			}
			info, _ := d.Info()
			rel, _ := filepath.Rel(p, path)
			if info != nil {
				fmt.Fprintf(&b, "%s %8d  %s\n", info.Mode().String(), info.Size(), rel)
			} else {
				fmt.Fprintf(&b, "%s\n", rel)
			}
			n++
			return nil
		})
	} else {
		entries, rerr := os.ReadDir(p)
		if rerr != nil {
			return ErrorResult("list %s: %v", p, rerr), nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if n >= max {
				truncated = true
				break
			}
			info, _ := e.Info()
			suffix := ""
			if e.IsDir() {
				suffix = "/"
			}
			if info != nil {
				fmt.Fprintf(&b, "%s %8d  %s%s\n", info.Mode().String(), info.Size(), e.Name(), suffix)
			} else {
				fmt.Fprintf(&b, "%s%s\n", e.Name(), suffix)
			}
			n++
		}
	}
	if err != nil {
		return ErrorResult("list %s: %v", p, err), nil
	}
	if b.Len() == 0 {
		return TextResult("(empty directory)"), nil
	}
	if truncated {
		b.WriteString("\n... [entry limit reached] ...\n")
	}
	return Result{Content: b.String()}, nil
}

// SearchFilesTool searches names and contents. It prefers ripgrep and falls
// back to a pure-Go walker when rg is unavailable.
type SearchFilesTool struct {
	Roots     []string
	Workspace string
}

func (t *SearchFilesTool) Name() string { return "search_files" }
func (t *SearchFilesTool) Description() string {
	return "Search file contents by regular expression and/or file names by glob (supports **). Uses ripgrep when installed, otherwise a pure-Go fallback."
}
func (t *SearchFilesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"Directory to search, defaults to workspace."},
			"query":{"type":"string","description":"Regular expression for file contents."},
			"glob":{"type":"string","description":"File name glob, e.g. '*.go' or '**/*.md'."},
			"case_sensitive":{"type":"boolean"},
			"max_results":{"type":"integer","description":"Default 100, max 500."}
		}
	}`)
}

func (t *SearchFilesTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Path          string `json:"path"`
		Query         string `json:"query"`
		Glob          string `json:"glob"`
		CaseSensitive bool   `json:"case_sensitive"`
		MaxResults    int    `json:"max_results"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	if a.Query == "" && a.Glob == "" {
		return ErrorResult("provide query, glob, or both"), nil
	}
	if a.Path == "" {
		a.Path = t.Workspace
		if a.Path == "" && len(t.Roots) > 0 {
			a.Path = t.Roots[0]
		}
	}
	p, err := SecurePath(t.Roots, a.Path)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	max := a.MaxResults
	if max <= 0 {
		max = 100
	}
	if max > 500 {
		max = 500
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		args := []string{"--no-heading", "--line-number", "--color", "never", "--max-count", "20"}
		if !a.CaseSensitive {
			args = append(args, "-i")
		}
		if a.Glob != "" {
			args = append(args, "--glob", a.Glob)
		}
		if a.Query != "" {
			args = append(args, "--", a.Query, p)
		} else {
			args = append(args, "--files", p)
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, rg, args...)
		out, _ := cmd.Output()
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 1 && lines[0] == "" {
			return TextResult("no matches"), nil
		}
		if len(lines) > max {
			lines = lines[:max]
			lines = append(lines, fmt.Sprintf("... [limited to %d results] ...", max))
		}
		return Result{Content: strings.Join(lines, "\n")}, nil
	}
	return t.fallback(ctx, p, a.Query, a.Glob, a.CaseSensitive, max)
}

func (t *SearchFilesTool) fallback(ctx context.Context, root, query, glob string, caseSensitive bool, max int) (Result, error) {
	var re *regexp.Regexp
	var err error
	if query != "" {
		pat := query
		if !caseSensitive {
			pat = "(?i)" + pat
		}
		re, err = regexp.Compile(pat)
		if err != nil {
			return ErrorResult("invalid regex: %v", err), nil
		}
	}
	var globRe *regexp.Regexp
	if glob != "" {
		globRe = globRegexp(glob)
	}
	var b strings.Builder
	count, scanned := 0, 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == ".venv" || base == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		scanned++
		if scanned > 20000 || count >= max {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, path)
		if globRe != nil {
			name := filepath.ToSlash(rel)
			if !globRe.MatchString(name) && !globRe.MatchString(d.Name()) {
				return nil
			}
		}
		if re == nil {
			fmt.Fprintf(&b, "%s\n", rel)
			count++
			return nil
		}
		data, err := readSmall(path, 512*1024)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if re.MatchString(line) {
				fmt.Fprintf(&b, "%s:%d: %s\n", rel, lineNo, truncateLine(line, 300))
				count++
				if count >= max {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		return ErrorResult("search: %v", walkErr), nil
	}
	if count == 0 {
		return TextResult("no matches"), nil
	}
	if count >= max {
		fmt.Fprintf(&b, "... [limited to %d results] ...\n", max)
	}
	return Result{Content: b.String()}, nil
}

func readSmall(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func globRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	g := filepath.ToSlash(glob)
	for i := 0; i < len(g); i++ {
		switch c := g[i]; c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				b.WriteString(".*")
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '.', '+', '(', ')', '|', '^', '$', '[', ']', '{', '}', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
