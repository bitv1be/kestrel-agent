package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SecurePath resolves p inside one of roots. Relative paths are resolved
// against the first root. Existing symlinks are evaluated so a link cannot
// escape the sandbox.
func SecurePath(roots []string, p string) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf("no file roots configured")
	}
	if strings.TrimSpace(p) == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(roots[0], p)
	}
	abs, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	for _, root := range roots {
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if within(rootAbs, abs) {
			// Follow symlinks for existing components and re-check.
			resolved := abs
			if r, err := filepath.EvalSymlinks(abs); err == nil {
				resolved = r
			} else if r, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
				resolved = filepath.Join(r, filepath.Base(abs))
			}
			if !within(rootAbs, resolved) {
				return "", fmt.Errorf("path %q escapes the allowed roots via symlink", p)
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed roots (%s)", p, strings.Join(roots, ", "))
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// Within reports whether p is inside root.
func Within(root, p string) bool {
	r, err1 := filepath.Abs(root)
	a, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	return within(r, a)
}

// readLimited reads at most max bytes from path.
func readLimited(path string, max int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	if max <= 0 {
		max = 256 * 1024
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, false, err
	}
	truncated := int64(len(data)) > max
	if truncated {
		data = data[:max]
	}
	return data, truncated, nil
}
