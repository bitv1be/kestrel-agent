package telegram

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Telegram accepts at most 4096 characters per message (after entity parsing).
const telegramMaxMessage = 4096

// formatHTML converts the small Markdown subset Kestrel emits into the HTML
// dialect accepted by Telegram. Every text fragment is escaped, so model output
// cannot inject markup. Supported: fenced code, inline code, bold, italic,
// strikethrough, links, headings, blockquotes and bullet lists.
func formatHTML(md string) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = strings.ReplaceAll(md, "\r", "\n")
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			var body []string
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				body = append(body, lines[j])
				j++
			}
			class := ""
			if validLang(lang) {
				class = ` class="language-` + lang + `"`
			}
			out = append(out, "<pre><code"+class+">"+html.EscapeString(strings.Join(body, "\n"))+"</code></pre>")
			if j < len(lines) {
				j++
			}
			i = j
			continue
		}
		if heading, ok := markdownHeading(trimmed); ok {
			out = append(out, "<b>"+formatInline(heading)+"</b>")
			i++
			continue
		}
		if isHorizontalRule(trimmed) {
			out = append(out, "──────────")
			i++
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			var quote []string
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if !strings.HasPrefix(t, ">") {
					break
				}
				t = strings.TrimPrefix(t, ">")
				t = strings.TrimPrefix(t, " ")
				quote = append(quote, t)
				i++
			}
			out = append(out, "<blockquote>"+formatInline(strings.Join(quote, "\n"))+"</blockquote>")
			continue
		}
		if item, ok := markdownBullet(line); ok {
			out = append(out, "• "+formatInline(item))
			i++
			continue
		}
		out = append(out, formatInline(line))
		i++
	}
	return strings.Join(out, "\n")
}

var headingRE = regexp.MustCompile(`^(#{1,6})\s+(.*\S)\s*$`)
var bulletRE = regexp.MustCompile(`^\s*[-*+]\s+(.*\S)\s*$`)

func markdownHeading(s string) (string, bool) {
	m := headingRE.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	text := m[2]
	// A leading "#" sequence without a space is not a heading.
	return text, true
}

func markdownBullet(s string) (string, bool) {
	// Avoid treating "---" and table separators as bullets.
	if isHorizontalRule(strings.TrimSpace(s)) {
		return "", false
	}
	m := bulletRE.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func isHorizontalRule(s string) bool {
	if len(s) < 3 {
		return false
	}
	c := s[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}

func validLang(s string) bool {
	if s == "" || len(s) > 24 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// formatInline converts inline Markdown, escaping everything else.
func formatInline(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '`' {
			if end := strings.IndexByte(s[i+1:], '`'); end >= 0 {
				b.WriteString("<code>" + html.EscapeString(s[i+1:i+1+end]) + "</code>")
				i += end + 2
				continue
			}
		}
		if s[i] == '[' {
			label, target, next, ok := markdownLink(s, i)
			if ok {
				if safeLink(target) {
					b.WriteString(`<a href="` + html.EscapeString(target) + `">` + formatInline(label) + `</a>`)
				} else {
					b.WriteString(formatInline(label))
				}
				i = next
				continue
			}
		}
		if inner, next, ok := delimited(s, i, "**"); ok {
			b.WriteString("<b>" + formatInline(inner) + "</b>")
			i = next
			continue
		}
		if inner, next, ok := delimited(s, i, "__"); ok {
			b.WriteString("<b>" + formatInline(inner) + "</b>")
			i = next
			continue
		}
		if inner, next, ok := delimited(s, i, "~~"); ok {
			b.WriteString("<s>" + formatInline(inner) + "</s>")
			i = next
			continue
		}
		if s[i] == '*' || s[i] == '_' {
			marker := s[i : i+1]
			if marker == "_" && i > 0 && isWordByte(s[i-1]) {
				// Underscores inside identifiers (file_name) are literal.
				b.WriteString(marker)
				i++
				continue
			}
			if inner, next, ok := delimited(s, i, marker); ok {
				b.WriteString("<i>" + formatInline(inner) + "</i>")
				i = next
				continue
			}
		}
		switch s[i] {
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '&':
			b.WriteString("&amp;")
		default:
			b.WriteByte(s[i])
		}
		i++
	}
	return b.String()
}

func delimited(s string, i int, marker string) (inner string, next int, ok bool) {
	if !strings.HasPrefix(s[i:], marker) {
		return "", 0, false
	}
	start := i + len(marker)
	end := strings.Index(s[start:], marker)
	if end <= 0 {
		return "", 0, false
	}
	// Emphasis must not swallow the remainder of a huge line by accident.
	inner = s[start : start+end]
	if strings.TrimSpace(inner) == "" {
		return "", 0, false
	}
	next = start + end + len(marker)
	return inner, next, true
}

func markdownLink(s string, i int) (label, target string, next int, ok bool) {
	if i >= len(s) || s[i] != '[' {
		return "", "", 0, false
	}
	closeBracket := strings.IndexByte(s[i+1:], ']')
	if closeBracket < 0 {
		return "", "", 0, false
	}
	label = s[i+1 : i+1+closeBracket]
	open := i + 1 + closeBracket + 1
	if open >= len(s) || s[open] != '(' {
		return "", "", 0, false
	}
	depth := 1
	for j := open + 1; j < len(s); j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				target = strings.TrimSpace(s[open+1 : j])
				if target == "" {
					return "", "", 0, false
				}
				// Drop an optional link title: [text](url "title")
				if k := strings.IndexAny(target, " \t"); k > 0 {
					target = target[:k]
				}
				return label, target, j + 1, true
			}
		}
	}
	return "", "", 0, false
}

func safeLink(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	for _, scheme := range []string{"http://", "https://", "mailto:", "tg://"} {
		if strings.HasPrefix(l, scheme) {
			return true
		}
	}
	return false
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

var tagRE = regexp.MustCompile(`<[^>]*>`)

// stripTags turns formatted HTML back into plain text.
func stripTags(s string) string {
	return html.UnescapeString(tagRE.ReplaceAllString(s, ""))
}

// splitMarkdown splits Markdown into chunks that fit Telegram. Code fences are
// closed and reopened so each chunk renders independently. Chunks are kept
// below the hard limit to leave room for formatting tags.
func splitMarkdown(md string, max int) []string {
	if max <= 0 || max > telegramMaxMessage {
		max = telegramMaxMessage
	}
	budget := max - 250
	if budget < max/2 {
		budget = max / 2
	}
	md = strings.ReplaceAll(md, "\r\n", "\n")
	md = strings.ReplaceAll(md, "\r", "\n")
	lines := strings.SplitAfter(md, "\n")
	var chunks []string
	var cur strings.Builder
	curLen := 0
	fence := ""

	flush := func() {
		s := strings.TrimRight(cur.String(), "\n")
		if fence != "" {
			s += "\n```"
		}
		if strings.TrimSpace(s) != "" {
			chunks = append(chunks, s)
		}
		cur.Reset()
		curLen = 0
		if fence != "" {
			cur.WriteString(fence)
			cur.WriteByte('\n')
			curLen = utf8.RuneCountInString(fence) + 1
		}
	}

	for _, line := range lines {
		if line == "" {
			continue
		}
		n := utf8.RuneCountInString(line)
		if curLen > 0 && curLen+n > budget {
			flush()
		}
		// Hard-split a single very long line.
		for utf8.RuneCountInString(line) > budget-curLen {
			room := budget - curLen
			if room < 64 {
				flush()
				room = budget
			}
			head, rest := takeRunes(line, room)
			cur.WriteString(head)
			curLen += utf8.RuneCountInString(head)
			flush()
			line = rest
			if line == "" {
				break
			}
		}
		if line == "" {
			continue
		}
		cur.WriteString(line)
		curLen += utf8.RuneCountInString(line)
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if fence == "" {
				fence = trimmed
			} else {
				fence = ""
			}
		}
	}
	if cur.Len() > 0 {
		s := strings.TrimRight(cur.String(), "\n")
		if fence != "" {
			s += "\n```"
		}
		if strings.TrimSpace(s) != "" {
			chunks = append(chunks, s)
		}
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

// streamHTML renders an in-progress message. Oversized streams show a tail
// preview escaped as plain text so no partial markup reaches Telegram.
func streamHTML(md string, max int) string {
	if max <= 0 || max > telegramMaxMessage {
		max = telegramMaxMessage
	}
	src := md
	if utf8.RuneCountInString(src) > max*3 {
		src = tailRunes(src, max*3)
	}
	formatted := formatHTML(src)
	if utf8.RuneCountInString(formatted) <= max {
		return formatted
	}
	plain := stripTags(formatted)
	plain = tailRunes(plain, max/6-8)
	return "<i>…</i>" + html.EscapeString(plain)
}

func takeRunes(s string, n int) (string, string) {
	if n <= 0 {
		return "", s
	}
	r := []rune(s)
	if len(r) <= n {
		return s, ""
	}
	return string(r[:n]), string(r[n:])
}

func tailRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
