package tools

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxSkillDownloadBytes = 32 << 20
	maxSkillExtractBytes  = 64 << 20
	maxSkillFileBytes     = 16 << 20
	maxSkillFiles         = 2000
	maxSkillMarkdownBytes = 1 << 20
)

type installRequest struct {
	URL   string
	Path  string
	Skill string
	Force bool
}

// materialized is a local tree ready to be searched for SKILL.md files.
type materialized struct {
	root     string
	selector string
}

type skillCandidate struct {
	Dir  string
	File string
	Name string
	Rel  string
}

func (t *SkillsTool) install(ctx context.Context, req installRequest) (Result, error) {
	if strings.TrimSpace(t.Dir) == "" {
		return ErrorResult("skills directory is not configured"), nil
	}
	if strings.TrimSpace(req.URL) == "" && strings.TrimSpace(req.Path) == "" {
		return ErrorResult("url or path is required for action=install"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	temp, err := os.MkdirTemp("", "kestrel-skill-")
	if err != nil {
		return ErrorResult("create temporary directory: %v", err), nil
	}
	defer os.RemoveAll(temp)

	src, err := t.materialize(ctx, req, temp)
	if err != nil {
		return ErrorResult("install source: %v", err), nil
	}
	candidates, err := findSkillCandidates(src.root)
	if err != nil {
		return ErrorResult("scan source: %v", err), nil
	}
	selector := strings.TrimSpace(req.Skill)
	if selector == "" {
		selector = src.selector
	}
	chosen, err := selectSkillCandidate(candidates, selector, src.root)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	name := candidateName(chosen)
	target, err := t.targetDir(name)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	if Within(chosen.Dir, target) || Within(target, chosen.Dir) {
		return ErrorResult("source and destination overlap; install from a different location"), nil
	}
	if _, err := os.Stat(target); err == nil {
		if !req.Force {
			return ErrorResult("skill %q already exists; pass force=true to overwrite", name), nil
		}
		if err := os.RemoveAll(target); err != nil {
			return ErrorResult("replace existing skill: %v", err), nil
		}
	}
	if err := os.MkdirAll(t.Dir, 0o750); err != nil {
		return ErrorResult("create skills directory: %v", err), nil
	}
	files, bytesWritten, err := copySkillTree(chosen.Dir, target)
	if err != nil {
		_ = os.RemoveAll(target)
		return ErrorResult("install skill: %v", err), nil
	}
	rewriteSkillName(filepath.Join(target, "SKILL.md"), filepath.Base(target))
	source := strings.TrimSpace(req.URL)
	if source == "" {
		source = strings.TrimSpace(req.Path)
	}
	return TextResult("installed skill %q from %s at %s (%d files, %s)", name, source, target, files, humanBytes(bytesWritten)), nil
}

func (t *SkillsTool) targetDir(name string) (string, error) {
	if !skillNameRE.MatchString(name) {
		return "", fmt.Errorf("invalid skill name %q", name)
	}
	root, err := filepath.Abs(t.Dir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, name)
	if !Within(root, target) || target == root {
		return "", errors.New("skill path escapes the skills directory")
	}
	return target, nil
}

func (t *SkillsTool) materialize(ctx context.Context, req installRequest, temp string) (materialized, error) {
	pathArg := strings.TrimSpace(req.Path)
	urlArg := strings.TrimSpace(req.URL)

	if pathArg != "" {
		local, err := t.resolveLocalPath(pathArg)
		if err == nil {
			if st, statErr := os.Stat(local); statErr == nil {
				if st.IsDir() {
					return materialized{root: local}, nil
				}
				return t.materializeFile(local, temp, "")
			}
		}
		if urlArg == "" {
			if target, ok := parseRepoRef(pathArg); ok {
				return t.materializeGitHub(ctx, target, temp)
			}
		}
		if err != nil {
			return materialized{}, err
		}
		return materialized{}, fmt.Errorf("path %q does not exist", pathArg)
	}

	// Accept host/path shorthand without a scheme.
	lowerURL := strings.ToLower(urlArg)
	for _, prefix := range []string{"github.com/", "www.github.com/", "skills.sh/", "www.skills.sh/", "raw.githubusercontent.com/"} {
		if strings.HasPrefix(lowerURL, prefix) {
			urlArg = "https://" + urlArg
			break
		}
	}

	if target, ok := parseRepoRef(urlArg); ok {
		return t.materializeGitHub(ctx, target, temp)
	}
	u, err := url.Parse(urlArg)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return materialized{}, fmt.Errorf("invalid install URL %q", urlArg)
	}

	switch strings.ToLower(u.Hostname()) {
	case "skills.sh", "www.skills.sh":
		parts := splitURLPath(u.Path)
		if len(parts) < 2 {
			return materialized{}, errors.New("skills.sh URL must look like skills.sh/<owner>/<repo>/<skill>")
		}
		target := githubTarget{owner: parts[0], repo: parts[1]}
		if len(parts) > 2 {
			target.selector = strings.Join(parts[2:], "/")
		}
		return t.materializeGitHub(ctx, target, temp)
	case "github.com", "www.github.com":
		target, err := parseGitHubURL(u)
		if err != nil {
			return materialized{}, err
		}
		return t.materializeGitHub(ctx, target, temp)
	case "raw.githubusercontent.com":
		dest := filepath.Join(temp, "download.md")
		if err := t.download(ctx, u.String(), dest); err != nil {
			return materialized{}, err
		}
		name := strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path))
		if strings.EqualFold(name, "skill") {
			name = path.Base(path.Dir(u.Path))
		}
		return t.materializeMarkdownFile(dest, temp, name)
	}

	base := path.Base(u.Path)
	if strings.HasSuffix(strings.ToLower(base), ".md") {
		dest := filepath.Join(temp, "download.md")
		if err := t.download(ctx, u.String(), dest); err != nil {
			return materialized{}, err
		}
		name := strings.TrimSuffix(base, path.Ext(base))
		if strings.EqualFold(name, "skill") {
			name = path.Base(path.Dir(u.Path))
		}
		return t.materializeMarkdownFile(dest, temp, name)
	}

	dest := filepath.Join(temp, "download")
	if err := t.download(ctx, u.String(), dest); err != nil {
		return materialized{}, err
	}
	return t.materializeFile(dest, temp, strings.TrimSuffix(base, path.Ext(base)))
}

func (t *SkillsTool) materializeFile(local, temp, suggested string) (materialized, error) {
	if kind := detectArchive(local); kind != "" {
		dir := filepath.Join(temp, "extracted")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return materialized{}, err
		}
		if err := extractArchive(local, dir, kind); err != nil {
			return materialized{}, err
		}
		return materialized{root: archiveRoot(dir)}, nil
	}
	return t.materializeMarkdownFile(local, temp, suggested)
}

func (t *SkillsTool) materializeMarkdownFile(local, temp, suggested string) (materialized, error) {
	data, truncated, err := readLimited(local, maxSkillMarkdownBytes)
	if err != nil {
		return materialized{}, err
	}
	if truncated {
		return materialized{}, fmt.Errorf("markdown skill exceeds %s", humanBytes(maxSkillMarkdownBytes))
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return materialized{}, errors.New("file is not a markdown skill")
	}
	name := skillFrontmatterName(data)
	if name == "" {
		name = suggested
	}
	if name == "" {
		name = skillNameFromFile(local)
	}
	name = safeSkillName(name)
	if name == "" {
		name = "skill"
	}
	dir := filepath.Join(temp, "single", name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return materialized{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), data, 0o644); err != nil {
		return materialized{}, err
	}
	return materialized{root: filepath.Join(temp, "single"), selector: name}, nil
}

func (t *SkillsTool) resolveLocalPath(raw string) (string, error) {
	if raw == "~" || strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		raw = filepath.Join(home, strings.TrimLeft(raw[1:], `/\`))
	}
	if !filepath.IsAbs(raw) && t.Deps.Config != nil && t.Deps.Config.Workspace != "" {
		raw = filepath.Join(t.Deps.Config.Workspace, raw)
	}
	return filepath.Abs(raw)
}

// githubTarget identifies a repository, optional git ref and optional path
// inside the repository.
type githubTarget struct {
	owner    string
	repo     string
	ref      string
	subdir   string
	file     string
	selector string
}

func parseRepoRef(raw string) (githubTarget, bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return githubTarget{}, false
	}
	parts := splitURLPath(strings.ReplaceAll(raw, `\`, "/"))
	if len(parts) < 2 {
		return githubTarget{}, false
	}
	target := githubTarget{owner: parts[0], repo: strings.TrimSuffix(parts[1], ".git")}
	if at := strings.LastIndexByte(target.repo, '@'); at > 0 {
		target.ref = target.repo[at+1:]
		target.repo = target.repo[:at]
	}
	if len(parts) > 2 {
		target.subdir = strings.Join(parts[2:], "/")
	}
	if !validGitHubPart(target.owner) || !validGitHubPart(target.repo) || !validGitRef(target.ref) {
		return githubTarget{}, false
	}
	return target, true
}

func parseGitHubURL(u *url.URL) (githubTarget, error) {
	parts := splitURLPath(u.Path)
	if len(parts) < 2 {
		return githubTarget{}, errors.New("GitHub URL must include an owner and repository")
	}
	target := githubTarget{owner: parts[0], repo: strings.TrimSuffix(parts[1], ".git")}
	switch {
	case len(parts) >= 4 && parts[2] == "tree":
		target.ref = parts[3]
		target.subdir = strings.Join(parts[4:], "/")
	case len(parts) >= 4 && parts[2] == "blob":
		target.ref = parts[3]
		target.file = strings.Join(parts[4:], "/")
	case len(parts) > 2:
		target.subdir = strings.Join(parts[2:], "/")
	}
	if !validGitHubPart(target.owner) || !validGitHubPart(target.repo) || !validGitRef(target.ref) {
		return githubTarget{}, errors.New("invalid GitHub repository URL")
	}
	if target.file != "" && target.ref == "" {
		return githubTarget{}, errors.New("GitHub blob URL is missing a branch")
	}
	return target, nil
}

func validGitHubPart(s string) bool {
	return s != "" && s != "." && s != ".." &&
		!strings.ContainsAny(s, `/\`) && !strings.ContainsAny(s, " :?#%")
}

func validGitRef(ref string) bool {
	if ref == "" {
		return true
	}
	if strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "..") {
		return false
	}
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == '/':
		default:
			return false
		}
	}
	return true
}

func (t *SkillsTool) materializeGitHub(ctx context.Context, target githubTarget, temp string) (materialized, error) {
	if target.file != "" {
		raw := rawGitHubURL(target)
		dest := filepath.Join(temp, "download.md")
		if err := t.download(ctx, raw, dest); err != nil {
			return materialized{}, err
		}
		name := path.Base(target.file)
		if strings.EqualFold(name, "SKILL.md") {
			name = path.Base(path.Dir(target.file))
		} else {
			name = strings.TrimSuffix(name, path.Ext(name))
		}
		return t.materializeMarkdownFile(dest, temp, name)
	}

	dest := filepath.Join(temp, "repo.tar.gz")
	if err := t.downloadGitHubArchive(ctx, target, dest); err != nil {
		return materialized{}, err
	}
	dir := filepath.Join(temp, "repo")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return materialized{}, err
	}
	if err := extractArchive(dest, dir, "tar.gz"); err != nil {
		return materialized{}, err
	}
	root := archiveRoot(dir)
	if target.subdir != "" {
		candidate := filepath.Join(root, filepath.FromSlash(target.subdir))
		if !Within(root, candidate) {
			return materialized{}, errors.New("GitHub path escapes the repository")
		}
		st, err := os.Stat(candidate)
		if err != nil || !st.IsDir() {
			return materialized{}, fmt.Errorf("path %q not found in repository", target.subdir)
		}
		root = candidate
	}
	return materialized{root: root, selector: target.selector}, nil
}

func (t *SkillsTool) downloadGitHubArchive(ctx context.Context, target githubTarget, dest string) error {
	ref := target.ref
	var endpoint string
	if t.githubToken() != "" {
		endpoint = githubAPIBase + "/repos/" + url.PathEscape(target.owner) + "/" + url.PathEscape(target.repo) + "/tarball"
		if ref != "" && !strings.EqualFold(ref, "HEAD") {
			endpoint += "/" + escapeRef(ref)
		}
	} else {
		if ref == "" {
			ref = "HEAD"
		}
		endpoint = githubCodeloadBase + "/" + url.PathEscape(target.owner) + "/" + url.PathEscape(target.repo) + "/tar.gz/" + escapeRef(ref)
	}
	if err := t.download(ctx, endpoint, dest); err != nil {
		return err
	}
	return nil
}

func rawGitHubURL(target githubTarget) string {
	segments := strings.Split(target.file, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return "https://raw.githubusercontent.com/" + url.PathEscape(target.owner) + "/" +
		url.PathEscape(target.repo) + "/" + escapeRef(target.ref) + "/" + strings.Join(segments, "/")
}

func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (t *SkillsTool) download(ctx context.Context, endpoint, dest string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid download URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", t.userAgent())
	req.Header.Set("Accept", "*/*")
	if strings.EqualFold(u.Hostname(), "api.github.com") {
		if token := t.githubToken(); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		}
	}
	client, err := t.httpClient(2 * time.Minute)
	if err != nil {
		return fmt.Errorf("build HTTP client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", u.Hostname(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		detail := strings.TrimSpace(string(body))
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return fmt.Errorf("download %s: HTTP %d %s", u.Hostname(), resp.StatusCode, detail)
	}
	if resp.ContentLength > maxSkillDownloadBytes {
		return fmt.Errorf("download is too large (%s, max %s)", humanBytes(resp.ContentLength), humanBytes(maxSkillDownloadBytes))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxSkillDownloadBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxSkillDownloadBytes {
		_ = os.Remove(dest)
		return fmt.Errorf("download exceeds %s", humanBytes(maxSkillDownloadBytes))
	}
	return nil
}

func findSkillCandidates(root string) ([]skillCandidate, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var out []skillCandidate
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(d.Name(), "SKILL.md") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		info, _ := parseSkillFile(p)
		out = append(out, skillCandidate{
			Dir:  filepath.Dir(p),
			File: p,
			Name: info.Name,
			Rel:  filepath.ToSlash(rel),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, nil
}

func selectSkillCandidate(cands []skillCandidate, selector, root string) (skillCandidate, error) {
	if len(cands) == 0 {
		return skillCandidate{}, errors.New("no SKILL.md found; expected a skill folder or a markdown file")
	}
	selector = strings.Trim(strings.TrimSpace(selector), "/")
	selector = strings.TrimPrefix(selector, "./")
	if selector != "" {
		for _, c := range cands {
			relDir, _ := filepath.Rel(root, c.Dir)
			relDir = filepath.ToSlash(relDir)
			switch {
			case strings.EqualFold(c.Name, selector),
				strings.EqualFold(filepath.Base(c.Dir), selector),
				strings.EqualFold(relDir, selector),
				strings.EqualFold(c.Rel, selector+"/SKILL.md"):
				return c, nil
			}
		}
		return skillCandidate{}, fmt.Errorf("skill %q not found; available: %s", selector, candidateList(cands, root))
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	return skillCandidate{}, fmt.Errorf("source contains %d skills: %s. Call install again with skill=<name>", len(cands), candidateList(cands, root))
}

func candidateList(cands []skillCandidate, root string) string {
	names := make([]string, 0, len(cands))
	for i, c := range cands {
		if i >= 12 {
			names = append(names, fmt.Sprintf("… and %d more", len(cands)-12))
			break
		}
		rel, _ := filepath.Rel(root, c.Dir)
		names = append(names, filepath.ToSlash(rel))
	}
	return strings.Join(names, ", ")
}

func candidateName(c skillCandidate) string {
	if skillNameRE.MatchString(c.Name) {
		return c.Name
	}
	name := safeSkillName(filepath.Base(c.Dir))
	if name == "" {
		name = "skill"
	}
	return name
}

func copySkillTree(src, dst string) (int, int64, error) {
	var files int
	var total int64
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o750)
		}
		target := filepath.Join(dst, rel)
		if !Within(dst, target) {
			return fmt.Errorf("path %q escapes the destination", rel)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to copy symlink %q", rel)
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o750)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file %q", rel)
		}
		if info.Size() > maxSkillFileBytes {
			return fmt.Errorf("file %q is too large (%s)", rel, humanBytes(info.Size()))
		}
		files++
		if files > maxSkillFiles {
			return fmt.Errorf("skill contains too many files (max %d)", maxSkillFiles)
		}
		if total+info.Size() > maxSkillExtractBytes {
			return fmt.Errorf("skill content exceeds %s", humanBytes(maxSkillExtractBytes))
		}
		mode := info.Mode().Perm()
		if mode&0o111 != 0 {
			mode = 0o755
		} else {
			mode |= 0o644
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			in.Close()
			return err
		}
		n, copyErr := io.Copy(out, io.LimitReader(in, maxSkillFileBytes+1))
		closeErr := out.Close()
		in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n > maxSkillFileBytes {
			return fmt.Errorf("file %q is too large", rel)
		}
		total += n
		if total > maxSkillExtractBytes {
			return fmt.Errorf("skill content exceeds %s", humanBytes(maxSkillExtractBytes))
		}
		return nil
	})
	return files, total, err
}

func rewriteSkillName(file, name string) {
	data, err := os.ReadFile(file)
	if err != nil || len(data) > 512*1024 {
		return
	}
	text := string(data)
	if !strings.HasPrefix(text, "---") {
		return
	}
	rest := text[3:]
	closeAt := strings.Index(rest, "\n---")
	if closeAt < 0 {
		return
	}
	lines := strings.Split(rest[:closeAt], "\n")
	changed := false
	for i, line := range lines {
		key, _, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "name") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(line, key+":")) != name {
			lines[i] = "name: " + name
			changed = true
		}
	}
	if !changed {
		return
	}
	updated := "---" + strings.Join(lines, "\n") + rest[closeAt:]
	_ = os.WriteFile(file, []byte(updated), 0o644)
}

func skillFrontmatterName(data []byte) string {
	text := string(data)
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	rest := text[3:]
	closeAt := strings.Index(rest, "\n---")
	if closeAt < 0 {
		return ""
	}
	for _, line := range strings.Split(rest[:closeAt], "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "name") {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

func skillNameFromFile(p string) string {
	base := filepath.Base(p)
	if strings.EqualFold(base, "SKILL.md") {
		return filepath.Base(filepath.Dir(p))
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func safeSkillName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	lastDash := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-._")
	if len(out) > 64 {
		out = out[:64]
	}
	if out == "" || !skillNameRE.MatchString(out) {
		return ""
	}
	return out
}

func detectArchive(p string) string {
	lower := strings.ToLower(p)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return "zip"
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(lower, ".tar"):
		return "tar"
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	switch {
	case len(head) >= 4 && bytes.Equal(head[:4], []byte("PK\x03\x04")):
		return "zip"
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return "tar.gz"
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return "tar"
	}
	return ""
}

func extractArchive(src, dest, kind string) error {
	switch kind {
	case "zip":
		return extractZip(src, dest)
	case "tar":
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		return extractTar(f, dest)
	case "tar.gz":
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("open gzip archive: %w", err)
		}
		defer gz.Close()
		return extractTar(gz, dest)
	default:
		return fmt.Errorf("unsupported archive type %q", kind)
	}
}

func extractZip(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	var total int64
	var files int
	for _, entry := range zr.File {
		target, err := safeArchivePath(dest, entry.Name)
		if err != nil {
			return err
		}
		if entry.FileInfo().Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive contains a symlink: %q", entry.Name)
		}
		if entry.FileInfo().IsDir() || strings.HasSuffix(entry.Name, "/") {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		}
		if !entry.FileInfo().Mode().IsRegular() {
			return fmt.Errorf("archive contains an unsupported entry: %q", entry.Name)
		}
		if entry.UncompressedSize64 > maxSkillFileBytes {
			return fmt.Errorf("archive entry %q is too large", entry.Name)
		}
		files++
		if files > maxSkillFiles {
			return fmt.Errorf("archive contains too many files (max %d)", maxSkillFiles)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		rc, err := entry.Open()
		if err != nil {
			return err
		}
		mode := entry.FileInfo().Mode().Perm()
		if mode&0o111 != 0 {
			mode = 0o755
		} else {
			mode |= 0o644
		}
		_, err = writeLimitedFile(rc, target, mode, &total)
		closeErr := rc.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	var total int64
	var files int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeArchivePath(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size > maxSkillFileBytes {
				return fmt.Errorf("archive entry %q is too large", hdr.Name)
			}
			files++
			if files > maxSkillFiles {
				return fmt.Errorf("archive contains too many files (max %d)", maxSkillFiles)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			mode := fs.FileMode(hdr.Mode).Perm()
			if mode&0o111 != 0 {
				mode = 0o755
			} else {
				mode |= 0o644
			}
			if _, err := writeLimitedFile(tr, target, mode, &total); err != nil {
				return err
			}
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("archive contains a link: %q", hdr.Name)
		default:
			return fmt.Errorf("archive contains an unsupported entry: %q", hdr.Name)
		}
	}
}

func writeLimitedFile(r io.Reader, target string, mode fs.FileMode, total *int64) (int64, error) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(r, maxSkillFileBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if n > maxSkillFileBytes {
		_ = os.Remove(target)
		return 0, fmt.Errorf("archive entry is too large")
	}
	*total += n
	if *total > maxSkillExtractBytes {
		_ = os.Remove(target)
		return 0, fmt.Errorf("archive content exceeds %s", humanBytes(maxSkillExtractBytes))
	}
	return n, nil
}

func safeArchivePath(root, name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", errors.New("archive contains an invalid path")
	}
	name = strings.ReplaceAll(name, `\`, "/")
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("archive path escapes the destination: %q", name)
		}
		if strings.Contains(part, ":") {
			return "", fmt.Errorf("archive contains an invalid path: %q", name)
		}
	}
	if strings.HasPrefix(name, "/") || filepath.IsAbs(filepath.FromSlash(name)) || filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("archive contains an absolute path: %q", name)
	}
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	if clean == "" || clean == "." {
		return root, nil
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	if !Within(root, target) {
		return "", fmt.Errorf("archive path escapes the destination: %q", name)
	}
	return target, nil
}

func archiveRoot(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return dir
	}
	return filepath.Join(dir, entries[0].Name())
}

func splitURLPath(p string) []string {
	var out []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
