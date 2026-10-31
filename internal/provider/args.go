package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var fenceRE = regexp.MustCompile("(?s)^```(?:json|tool_call|tool)?\\s*|\\s*```$")

// ParseToolArguments validates and, when necessary, repairs the JSON string
// produced by a model. Weak/local models frequently emit trailing commas,
// single quotes, fenced JSON, or unbalanced closing braces. If repair is not
// possible the error is returned to the model as a tool result so it can
// retry.
func ParseToolArguments(raw string) (json.RawMessage, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return json.RawMessage("{}"), nil
	}
	if json.Valid([]byte(s)) && (strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")) {
		return json.RawMessage(s), nil
	}
	s = fenceRE.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "{[<"); i >= 0 && i > 0 {
		s = s[i:]
	}
	if strings.HasPrefix(s, "<") {
		return nil, fmt.Errorf("tool arguments are not JSON: %.200s", s)
	}
	candidates := []string{
		s,
		removeTrailingCommas(s),
		balanceJSON(removeTrailingCommas(s)),
		repairQuotes(removeTrailingCommas(s)),
		balanceJSON(repairQuotes(removeTrailingCommas(s))),
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if json.Valid([]byte(c)) {
			return json.RawMessage(c), nil
		}
	}
	return nil, fmt.Errorf("invalid tool arguments JSON: %.200s", raw)
}

func removeTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// balanceJSON appends missing closing delimiters, ignoring delimiters inside
// strings. It never removes characters.
func balanceJSON(s string) string {
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) == 0 {
				continue
			}
			open := stack[len(stack)-1]
			if (open == '{' && c == '}') || (open == '[' && c == ']') {
				stack = stack[:len(stack)-1]
			}
		}
	}
	var b strings.Builder
	b.WriteString(s)
	if inStr {
		b.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			b.WriteByte('}')
		} else {
			b.WriteByte(']')
		}
	}
	return b.String()
}

// repairQuotes converts single-quoted keys/strings to JSON double quotes. It
// is a best-effort pass and is only accepted if the result validates.
func repairQuotes(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	inDouble := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inDouble {
			b.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inDouble = false
			}
			continue
		}
		if c == '"' {
			inDouble = true
			b.WriteByte(c)
			continue
		}
		if c == '\'' {
			b.WriteByte('"')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func normalizeArguments(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}
