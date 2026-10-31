package tools

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bitvibe/kestrel/internal/config"
)

type recordedRequest struct {
	Path          string
	Query         string
	Authorization string
}

type requestRecorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *requestRecorder) add(req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, recordedRequest{
		Path:          req.URL.Path,
		Query:         req.URL.RawQuery,
		Authorization: req.Header.Get("Authorization"),
	})
	r.mu.Unlock()
}

func (r *requestRecorder) snapshot() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func executeSkills(t *testing.T, tool *SkillsTool, args map[string]any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute(%v): %v", args, err)
	}
	return res
}

func writeZipFile(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %q: %v", name, err)
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatalf("write zip entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func tarGzBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %q: %v", name, err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatalf("write tar entry %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSkillsCreateListLoadUpdate(t *testing.T) {
	tool := &SkillsTool{Dir: filepath.Join(t.TempDir(), "skills")}

	res := executeSkills(t, tool, map[string]any{
		"action":      "create",
		"name":        "pdf-tools",
		"description": "Work with PDF files",
		"content":     "# Steps\n\nUse pdftotext.",
	})
	if res.IsError {
		t.Fatalf("create: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(tool.Dir, "pdf-tools", "SKILL.md")); err != nil {
		t.Fatalf("created skill missing: %v", err)
	}

	res = executeSkills(t, tool, map[string]any{"action": "list"})
	if res.IsError || !strings.Contains(res.Content, "pdf-tools") || !strings.Contains(res.Content, "Work with PDF files") {
		t.Fatalf("list = %q (error=%v)", res.Content, res.IsError)
	}

	res = executeSkills(t, tool, map[string]any{"action": "load", "name": "pdf-tools"})
	if res.IsError || !strings.Contains(res.Content, "# Steps") || !strings.Contains(res.Content, "pdftotext") {
		t.Fatalf("load = %q (error=%v)", res.Content, res.IsError)
	}

	res = executeSkills(t, tool, map[string]any{
		"action": "create", "name": "pdf-tools", "description": "Duplicate", "content": "x",
	})
	if !res.IsError || !strings.Contains(res.Content, "already exists") {
		t.Fatalf("duplicate create = %q (error=%v)", res.Content, res.IsError)
	}

	res = executeSkills(t, tool, map[string]any{
		"action": "update", "name": "pdf-tools", "description": "Updated description", "content": "# Updated\n\nNew body.",
	})
	if res.IsError {
		t.Fatalf("update: %s", res.Content)
	}
	data, err := os.ReadFile(filepath.Join(tool.Dir, "pdf-tools", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Updated description") || !strings.Contains(string(data), "New body.") {
		t.Fatalf("updated skill contents = %q", data)
	}
}

func TestSkillsInstallLocalDirectory(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "source", "demo")
	if err := os.MkdirAll(filepath.Join(src, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: local-demo\ndescription: A local demo\n---\n\n# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &SkillsTool{Dir: filepath.Join(base, "skills")}
	res := executeSkills(t, tool, map[string]any{"action": "install", "path": filepath.Join(base, "source")})
	if res.IsError {
		t.Fatalf("install: %s", res.Content)
	}
	target := filepath.Join(tool.Dir, "local-demo")
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Fatalf("installed SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "scripts", "run.sh")); err != nil {
		t.Fatalf("installed script missing: %v", err)
	}

	res = executeSkills(t, tool, map[string]any{"action": "install", "path": filepath.Join(base, "source")})
	if !res.IsError || !strings.Contains(res.Content, "already exists") {
		t.Fatalf("duplicate install = %q (error=%v)", res.Content, res.IsError)
	}

	res = executeSkills(t, tool, map[string]any{"action": "install", "path": filepath.Join(base, "source"), "force": true})
	if res.IsError {
		t.Fatalf("forced install: %s", res.Content)
	}
}

func TestSkillsInstallMarkdownFile(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "download.md")
	doc := "---\nname: md-skill\ndescription: From markdown\n---\n\n# Body\n\nDo the thing.\n"
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := &SkillsTool{Dir: filepath.Join(base, "skills")}
	res := executeSkills(t, tool, map[string]any{"action": "install", "path": path})
	if res.IsError {
		t.Fatalf("install markdown: %s", res.Content)
	}
	data, err := os.ReadFile(filepath.Join(tool.Dir, "md-skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("installed markdown missing: %v", err)
	}
	if !strings.Contains(string(data), "Do the thing.") {
		t.Fatalf("installed markdown = %q", data)
	}
}

func TestSkillsInstallZipSelector(t *testing.T) {
	base := t.TempDir()
	zipPath := filepath.Join(base, "bundle.zip")
	writeZipFile(t, zipPath, map[string]string{
		"skills/alpha/SKILL.md": "---\nname: alpha\ndescription: Alpha skill\n---\n\n# Alpha\n",
		"skills/beta/SKILL.md":  "---\nname: beta\ndescription: Beta skill\n---\n\n# Beta\n",
	})

	tool := &SkillsTool{Dir: filepath.Join(base, "skills")}
	res := executeSkills(t, tool, map[string]any{"action": "install", "path": zipPath})
	if !res.IsError {
		t.Fatalf("multi-skill install should require a selector: %s", res.Content)
	}
	for _, want := range []string{"2 skills", "alpha", "beta", "skill=<name>"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("selector error %q missing %q", res.Content, want)
		}
	}

	res = executeSkills(t, tool, map[string]any{"action": "install", "path": zipPath, "skill": "beta"})
	if res.IsError {
		t.Fatalf("install selected skill: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(tool.Dir, "beta", "SKILL.md")); err != nil {
		t.Fatalf("selected skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tool.Dir, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("unselected skill should not be installed (stat err=%v)", err)
	}
}

func TestSkillsRejectZipSlipAndSymlink(t *testing.T) {
	base := t.TempDir()
	tool := &SkillsTool{Dir: filepath.Join(base, "skills")}

	slip := filepath.Join(base, "slip.zip")
	writeZipFile(t, slip, map[string]string{
		"../escape/SKILL.md": "---\nname: escape\ndescription: Escape\n---\n",
	})
	res := executeSkills(t, tool, map[string]any{"action": "install", "path": slip})
	if !res.IsError || !strings.Contains(res.Content, "escapes the destination") {
		t.Fatalf("zip-slip install = %q (error=%v)", res.Content, res.IsError)
	}
	if _, err := os.Stat(filepath.Join(base, "escape")); !os.IsNotExist(err) {
		t.Fatalf("zip-slip wrote outside the destination (stat err=%v)", err)
	}

	symlink := filepath.Join(base, "symlink.zip")
	f, err := os.Create(symlink)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	hdr := &zip.FileHeader{Name: "linked"}
	hdr.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, "SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	res = executeSkills(t, tool, map[string]any{"action": "install", "path": symlink})
	if !res.IsError || !strings.Contains(strings.ToLower(res.Content), "symlink") {
		t.Fatalf("symlink install = %q (error=%v)", res.Content, res.IsError)
	}
}

func TestSkillsInstallTarGzFromURL(t *testing.T) {
	archive := tarGzBytes(t, map[string]string{
		"package/SKILL.md":     "---\nname: url-skill\ndescription: From a URL\n---\n\n# URL skill\n",
		"package/README.md":    "readme",
		"package/scripts/a.sh": "#!/bin/sh\n",
	})
	rec := &requestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	tool := &SkillsTool{Dir: filepath.Join(t.TempDir(), "skills")}
	res := executeSkills(t, tool, map[string]any{"action": "install", "url": srv.URL + "/url-skill.tar.gz"})
	if res.IsError {
		t.Fatalf("install tar.gz: %s", res.Content)
	}
	target := filepath.Join(tool.Dir, "url-skill")
	data, err := os.ReadFile(filepath.Join(target, "SKILL.md"))
	if err != nil {
		t.Fatalf("installed tar skill missing: %v", err)
	}
	if !strings.Contains(string(data), "# URL skill") {
		t.Fatalf("installed tar skill = %q", data)
	}
	if _, err := os.Stat(filepath.Join(target, "README.md")); err != nil {
		t.Fatalf("installed tar file missing: %v", err)
	}
	reqs := rec.snapshot()
	if len(reqs) != 1 || reqs[0].Path != "/url-skill.tar.gz" {
		t.Fatalf("archive requests = %+v", reqs)
	}
}

func TestSkillsInstallGitHubRepository(t *testing.T) {
	archive := tarGzBytes(t, map[string]string{
		"repo-main/tool/SKILL.md": "---\nname: github-skill\ndescription: From GitHub\n---\n\n# GitHub\n",
	})
	rec := &requestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(archive)
	}))
	defer srv.Close()

	oldCodeload := githubCodeloadBase
	githubCodeloadBase = srv.URL
	defer func() { githubCodeloadBase = oldCodeload }()

	tool := &SkillsTool{Dir: filepath.Join(t.TempDir(), "skills")}
	res := executeSkills(t, tool, map[string]any{"action": "install", "url": "acme/skills"})
	if res.IsError {
		t.Fatalf("install GitHub repository: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(tool.Dir, "github-skill", "SKILL.md")); err != nil {
		t.Fatalf("installed GitHub skill missing: %v", err)
	}
	reqs := rec.snapshot()
	if len(reqs) != 1 || reqs[0].Path != "/acme/skills/tar.gz/HEAD" {
		t.Fatalf("codeload requests = %+v", reqs)
	}
}

func TestSkillsMarketplaceSearch(t *testing.T) {
	rec := &requestRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.URL.Path != "/search/repositories" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query().Get("q")
		items := `[{"full_name":"gamma/three","description":"Gamma skills","stargazers_count":7}]`
		if strings.Contains(q, "topic:agent-skills") {
			items = `[
				{"full_name":"alpha/one","description":"Alpha skills","stargazers_count":5},
				{"full_name":"beta/two","description":"Beta skills","stargazers_count":10}
			]`
		} else {
			items = `[
				{"full_name":"beta/two","description":"Beta skills updated","stargazers_count":12},
				{"full_name":"gamma/three","description":"Gamma skills","stargazers_count":7}
			]`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"items":%s}`, items)
	}))
	defer srv.Close()

	oldAPI := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = oldAPI }()

	cfg := config.Default()
	cfg.Skills.GitHubToken = "test-token"
	tool := &SkillsTool{Dir: filepath.Join(t.TempDir(), "skills"), Deps: Deps{Config: cfg}}
	res := executeSkills(t, tool, map[string]any{"action": "search", "query": "pdf"})
	if res.IsError {
		t.Fatalf("search: %s", res.Content)
	}
	beta := strings.Index(res.Content, "beta/two")
	gamma := strings.Index(res.Content, "gamma/three")
	alpha := strings.Index(res.Content, "alpha/one")
	if beta < 0 || gamma < beta || alpha < gamma {
		t.Fatalf("search results not sorted by deduped stars:\n%s", res.Content)
	}
	if strings.Count(res.Content, "- beta/two") != 1 {
		t.Fatalf("duplicate repository in results:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, `"action":"install"`) {
		t.Fatalf("search results lack install hints:\n%s", res.Content)
	}

	reqs := rec.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("search requests = %d, want 2", len(reqs))
	}
	for _, req := range reqs {
		if req.Authorization != "Bearer test-token" {
			t.Fatalf("authorization header = %q", req.Authorization)
		}
	}
}

func TestSkillsMarketplaceRateLimitFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	oldAPI := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = oldAPI }()

	tool := &SkillsTool{Dir: filepath.Join(t.TempDir(), "skills")}
	res := executeSkills(t, tool, map[string]any{"action": "search", "query": "anthropic"})
	if res.IsError {
		t.Fatalf("fallback search: %s", res.Content)
	}
	if !strings.Contains(res.Content, "anthropics/skills") {
		t.Fatalf("curated fallback missing:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "rate limited") || !strings.Contains(res.Content, "AGENT_SKILLS_GITHUB_TOKEN") {
		t.Fatalf("fallback lacks rate-limit context:\n%s", res.Content)
	}

	res = executeSkills(t, tool, map[string]any{"action": "search", "query": "no-such-skill-xyz"})
	if !res.IsError || !strings.Contains(res.Content, "rate limited") {
		t.Fatalf("empty fallback = %q (error=%v)", res.Content, res.IsError)
	}
}
