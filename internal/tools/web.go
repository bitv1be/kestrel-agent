package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// WebFetchTool fetches a URL through the resolved web proxy and converts HTML
// to readable text.
type WebFetchTool struct {
	Deps     Deps
	MaxBytes int
	Timeout  time.Duration
}

func (t *WebFetchTool) Name() string { return "web_fetch" }
func (t *WebFetchTool) Description() string {
	return "HTTP GET a public URL and return readable text. HTML is converted to plain text, scripts/styles are removed, and the response size is capped. Uses the configured web proxy."
}
func (t *WebFetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"url":{"type":"string","description":"http(s) URL to fetch."},
			"max_chars":{"type":"integer","description":"Maximum characters of text to return (default 30000)."}
		},
		"required":["url"]
	}`)
}

func (t *WebFetchTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrorResult("invalid http(s) URL"), nil
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	proxySpec := t.Deps.Config.ResolveProxy(t.Deps.Config.Tools.WebProxy, "")
	client, err := t.Deps.HTTP.Client(proxySpec, timeout)
	if err != nil {
		return ErrorResult("build HTTP client: %v", err), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ErrorResult("%v", err), nil
	}
	req.Header.Set("User-Agent", t.Deps.Config.HTTP.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return ErrorResult("fetch: %v", err), nil
	}
	defer resp.Body.Close()
	max := t.MaxBytes
	if max <= 0 {
		max = 2 << 20
	}
	limited := io.LimitReader(resp.Body, int64(max+1))
	body, err := io.ReadAll(limited)
	if err != nil {
		return ErrorResult("read response: %v", err), nil
	}
	if len(body) > max {
		body = body[:max]
	}
	maxChars := a.MaxChars
	if maxChars <= 0 {
		maxChars = 30000
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	var text string
	switch {
	case strings.Contains(ct, "text/html") || looksLikeHTML(body):
		text = htmlToText(body, u, maxChars)
	case strings.Contains(ct, "application/json"), strings.HasPrefix(ct, "text/"), strings.Contains(ct, "xml"):
		text = string(body)
	default:
		return ErrorResult("unsupported content type %q (%d bytes)", resp.Header.Get("Content-Type"), len(body)), nil
	}
	if len(text) > maxChars {
		text = text[:maxChars] + "\n... [truncated] ..."
	}
	head := fmt.Sprintf("URL: %s\nStatus: %d\n\n", resp.Request.URL.String(), resp.StatusCode)
	res := Result{Content: head + text}
	if resp.StatusCode >= 400 {
		res.IsError = true
	}
	return res, nil
}

func looksLikeHTML(b []byte) bool {
	s := strings.TrimSpace(strings.ToLower(string(b[:min(len(b), 512)])))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html") || strings.Contains(s, "<body")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// htmlToText extracts visible text, preserving basic block structure and
// appending link targets.
func htmlToText(data []byte, base *url.URL, maxChars int) string {
	doc, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return string(data)
	}
	var b strings.Builder
	var title string
	skip := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if b.Len() > maxChars*2 {
			return
		}
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if tag == "script" || tag == "style" || tag == "noscript" || tag == "svg" || tag == "head" {
				if tag == "head" {
					// Still capture <title>, then stop descending.
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						if c.Type == html.ElementNode && strings.EqualFold(c.Data, "title") && c.FirstChild != nil {
							title = strings.TrimSpace(c.FirstChild.Data)
						}
					}
				}
				return
			}
			switch tag {
			case "p", "div", "section", "article", "br", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "pre", "blockquote":
				b.WriteByte('\n')
				if tag == "li" {
					b.WriteString("- ")
				}
			case "a":
				for _, at := range n.Attr {
					if at.Key == "href" {
						defer func(href string) {
							if href != "" && !strings.HasPrefix(href, "#") && !strings.HasPrefix(href, "javascript:") {
								if ru, err := url.Parse(href); err == nil {
									abs := base.ResolveReference(ru).String()
									b.WriteString(" (" + abs + ")")
								}
							}
						}(at.Val)
					}
				}
			}
		}
		if n.Type == html.TextNode {
			text := n.Data
			if strings.TrimSpace(text) != "" {
				text = strings.Join(strings.Fields(text), " ")
				b.WriteString(text)
				if !strings.HasSuffix(b.String(), " ") {
					b.WriteByte(' ')
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode {
			switch strings.ToLower(n.Data) {
			case "p", "div", "section", "article", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "pre", "blockquote":
				b.WriteByte('\n')
			}
		}
	}
	walk(doc)
	out := b.String()
	// Collapse excess blank lines while preserving paragraphs.
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	out = strings.TrimSpace(out)
	if title != "" {
		out = "# " + title + "\n\n" + out
	}
	_ = skip
	return out
}

// WebSearchTool queries a pluggable search backend.
type WebSearchTool struct {
	Deps Deps
}

func (t *WebSearchTool) Name() string { return "web_search" }
func (t *WebSearchTool) Description() string {
	return "Search the web using the configured SearXNG or Brave backend and return titles, URLs and snippets."
}
func (t *WebSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"query":{"type":"string"},
			"max_results":{"type":"integer","description":"Default from config, max 20."}
		},
		"required":["query"]
	}`)
}

func (t *WebSearchTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var a struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := DecodeArgs(raw, &a); err != nil {
		return ErrorResult("%v", err), nil
	}
	q := strings.TrimSpace(a.Query)
	if q == "" {
		return ErrorResult("query is required"), nil
	}
	limit := a.MaxResults
	if limit <= 0 {
		limit = t.Deps.Config.Tools.SearchMaxResults
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}
	proxySpec := t.Deps.Config.ResolveProxy(t.Deps.Config.Tools.WebProxy, "")
	client, err := t.Deps.HTTP.Client(proxySpec, 30*time.Second)
	if err != nil {
		return ErrorResult("build HTTP client: %v", err), nil
	}
	switch strings.ToLower(t.Deps.Config.Tools.SearchBackend) {
	case "searxng", "searx":
		return t.searxng(ctx, client, q, limit)
	case "brave":
		return t.brave(ctx, client, q, limit)
	default:
		return ErrorResult("web search backend %q is not configured", t.Deps.Config.Tools.SearchBackend), nil
	}
}

func (t *WebSearchTool) searxng(ctx context.Context, client *http.Client, q string, limit int) (Result, error) {
	base := strings.TrimRight(t.Deps.Config.Tools.SearchURL, "/")
	if base == "" {
		return ErrorResult("tools.search_url is required for SearXNG"), nil
	}
	u := base + "/search?format=json&safesearch=0&q=" + url.QueryEscape(q)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("User-Agent", t.Deps.Config.HTTP.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ErrorResult("search: %v", err), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return ErrorResult("search HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), nil
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return ErrorResult("decode search response: %v", err), nil
	}
	return formatSearchResults(out.Results, limit), nil
}

func (t *WebSearchTool) brave(ctx context.Context, client *http.Client, q string, limit int) (Result, error) {
	if t.Deps.Config.Tools.SearchAPIKey == "" {
		return ErrorResult("tools.search_api_key is required for Brave"), nil
	}
	u := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d", url.QueryEscape(q), limit)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", t.Deps.Config.Tools.SearchAPIKey)
	req.Header.Set("User-Agent", t.Deps.Config.HTTP.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return ErrorResult("search: %v", err), nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return ErrorResult("search HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), nil
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return ErrorResult("decode search response: %v", err), nil
	}
	results := make([]struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	}, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		results = append(results, struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		}{Title: r.Title, URL: r.URL, Content: r.Description})
	}
	return formatSearchResults(results, limit), nil
}

func formatSearchResults(results []struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}, limit int) Result {
	if len(results) == 0 {
		return TextResult("no results")
	}
	if len(results) > limit {
		results = results[:limit]
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n\n", i+1, strings.TrimSpace(r.Title), r.URL, strings.TrimSpace(r.Content))
	}
	return Result{Content: strings.TrimSpace(b.String())}
}
