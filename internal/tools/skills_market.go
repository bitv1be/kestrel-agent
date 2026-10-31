package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// marketRepo is one marketplace search hit.
type marketRepo struct {
	FullName    string
	Description string
	Stars       int
}

// githubAPIBase is separated for tests so marketplace parsing can run offline.
var (
	githubAPIBase      = "https://api.github.com"
	githubCodeloadBase = "https://codeload.github.com"
)

// curatedSkillSources are useful defaults when GitHub's repository search
// returns nothing (for example because the API rate limit was reached).
var curatedSkillSources = []marketRepo{
	{FullName: "anthropics/skills", Description: "Official Anthropic Agent Skills collection"},
	{FullName: "vercel-labs/skills", Description: "Vercel's agent skill collection and examples"},
	{FullName: "ComposioHQ/awesome-claude-skills", Description: "Curated collection of Claude and agent skills"},
	{FullName: "travisvn/awesome-claude-skills", Description: "Community list of reusable agent skills"},
}

func (t *SkillsTool) search(ctx context.Context, query string) (Result, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return ErrorResult("query is required for action=search"), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()

	seen := make(map[string]marketRepo)
	var errs []string
	for _, suffix := range []string{" topic:agent-skills", " skill in:name,description"} {
		repos, err := t.searchGitHub(ctx, q+suffix)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for _, r := range repos {
			if strings.TrimSpace(r.FullName) == "" {
				continue
			}
			if old, ok := seen[r.FullName]; !ok || r.Stars > old.Stars {
				seen[r.FullName] = r
			}
		}
	}
	items := make([]marketRepo, 0, len(seen))
	for _, r := range seen {
		items = append(items, r)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Stars != items[j].Stars {
			return items[i].Stars > items[j].Stars
		}
		return items[i].FullName < items[j].FullName
	})
	if len(items) == 0 {
		lower := strings.ToLower(q)
		for _, c := range curatedSkillSources {
			if strings.Contains(strings.ToLower(c.FullName+" "+c.Description), lower) {
				items = append(items, c)
			}
		}
	}
	if len(items) > 10 {
		items = items[:10]
	}
	if len(items) == 0 {
		if len(errs) > 0 {
			return ErrorResult("marketplace search failed: %s%s", strings.Join(errs, "; "), t.githubTokenHint()), nil
		}
		return TextResult("No skill repositories found for %q. Try different words, or install directly with action=install and url=owner/repo.", q), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Skill repositories matching %q:\n", q)
	for _, r := range items {
		desc := strings.TrimSpace(r.Description)
		if desc == "" {
			desc = "no description"
		}
		fmt.Fprintf(&b, "- %s", r.FullName)
		if r.Stars > 0 {
			fmt.Fprintf(&b, " (%d stars)", r.Stars)
		}
		fmt.Fprintf(&b, ": %s\n  install: {\"action\":\"install\",\"url\":%q}\n", clipSkillText(desc, 240), r.FullName)
	}
	if len(errs) > 0 {
		fmt.Fprintf(&b, "\nNote: some marketplace queries failed: %s%s", strings.Join(errs, "; "), t.githubTokenHint())
	}
	b.WriteString("\nIf a repository has several skills, pass skill=<name> to choose one.")
	return Result{Content: strings.TrimSpace(b.String())}, nil
}

func (t *SkillsTool) searchGitHub(ctx context.Context, query string) ([]marketRepo, error) {
	endpoint := githubAPIBase + "/search/repositories?q=" + url.QueryEscape(query) +
		"&sort=stars&order=desc&per_page=8"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", t.userAgent())
	if token := t.githubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client, err := t.httpClient(30 * time.Second)
	if err != nil {
		return nil, fmt.Errorf("build HTTP client: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("GitHub search: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("GitHub search rate limited (HTTP %d)", resp.StatusCode)
		}
		return nil, fmt.Errorf("GitHub search HTTP %d: %s", resp.StatusCode, detail)
	}
	var result struct {
		Items []struct {
			FullName    string `json:"full_name"`
			Description string `json:"description"`
			Stars       int    `json:"stargazers_count"`
			Archived    bool   `json:"archived"`
			Fork        bool   `json:"fork"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("GitHub search: decode response: %w", err)
	}
	out := make([]marketRepo, 0, len(result.Items))
	for _, item := range result.Items {
		if item.Archived || item.Fork {
			continue
		}
		out = append(out, marketRepo{FullName: item.FullName, Description: item.Description, Stars: item.Stars})
	}
	return out, nil
}

func (t *SkillsTool) githubToken() string {
	if t.Deps.Config != nil {
		return strings.TrimSpace(t.Deps.Config.Skills.GitHubToken)
	}
	return ""
}

func (t *SkillsTool) githubTokenHint() string {
	if t.githubToken() != "" {
		return ""
	}
	return " Set skills.github_token or AGENT_SKILLS_GITHUB_TOKEN to raise GitHub API limits."
}

func (t *SkillsTool) userAgent() string {
	if t.Deps.Config != nil && strings.TrimSpace(t.Deps.Config.HTTP.UserAgent) != "" {
		return t.Deps.Config.HTTP.UserAgent
	}
	return "kestrel-agent"
}

func (t *SkillsTool) httpClient(timeout time.Duration) (*http.Client, error) {
	if t.Deps.HTTP != nil {
		spec := t.Deps.ProxySpec
		if t.Deps.Config != nil {
			spec = t.Deps.Config.ResolveProxy(t.Deps.Config.Tools.WebProxy, "")
		}
		return t.Deps.HTTP.Client(spec, timeout)
	}
	return &http.Client{Timeout: timeout}, nil
}

func clipSkillText(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
