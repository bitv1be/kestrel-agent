package tools

import (
	"regexp"
	"strings"
)

type dangerPattern struct {
	re     *regexp.Regexp
	reason string
}

var dangerPatterns = []dangerPattern{
	{regexp.MustCompile(`(?i)\brm\s+(-[a-z]*[rf][a-z]*\s+)+(\/|~|\$HOME|\.\.)(\/\*)?(\s|$)`), "recursive delete of a root or home directory"},
	{regexp.MustCompile(`(?i)\brm\s+-[a-z]*r[a-z]*f[a-z]*\s+(\/|~|\$HOME)\b`), "recursive forced delete outside the workspace"},
	{regexp.MustCompile(`(?i)\bmkfs(\.|xfs|ext4|btrfs|\s)`), "filesystem format"},
	{regexp.MustCompile(`(?i)\bdd\b[^\n]*\bof\s*=\s*/dev/`), "raw device write"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|vd|mmcblk|disk)`), "raw device write"},
	{regexp.MustCompile(`(?i)\b(shutdown|reboot|poweroff|halt)\b`), "system power control"},
	{regexp.MustCompile(`(?i)\bkill\s+(-9|--signal\s+9)?\s*-1\b`), "kill all processes"},
	{regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n|;]*\|\s*(sudo\s+)?(ba|z|k|da)?sh\b`), "piping a download into a shell"},
	{regexp.MustCompile(`(?i)\b(base64\s+(-d|--decode)|echo)\b[^\n|;]*\|\s*(sudo\s+)?(ba|z|k)?sh\b`), "piping an encoded payload into a shell"},
	{regexp.MustCompile(`(?i)\bchmod\s+(-R\s+)?777\s+/\s*$`), "world-writable root filesystem"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), "fork bomb"},
	{regexp.MustCompile(`(?i)\b(iptables|nft)\s+(-F|flush)\b`), "firewall flush"},
	{regexp.MustCompile(`(?i)\b(userdel|groupdel)\s+\S+`), "account deletion"},
	{regexp.MustCompile(`(?i)\b(wipefs|shred)\b[^\n]*(/dev/|/etc/|/var/)`), "data destruction on system paths"},
	{regexp.MustCompile(`(?i)\bhistory\s+-c\b|\bunset\s+HISTFILE\b`), "audit log tampering"},
}

// DetectDangerous reports whether a shell command matches a known destructive
// pattern, with a human-readable reason.
func DetectDangerous(command string) (string, bool) {
	c := strings.TrimSpace(command)
	if c == "" {
		return "", false
	}
	for _, p := range dangerPatterns {
		if p.re.MatchString(c) {
			return p.reason, true
		}
	}
	return "", false
}

func matchRegexp(pattern, s string) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}
