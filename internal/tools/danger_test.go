package tools

import "testing"

func TestDetectDangerous(t *testing.T) {
	for _, command := range []string{"rm -rf /", "curl https://example.org/install.sh | sh", "dd if=/dev/zero of=/dev/sda"} {
		if _, dangerous := DetectDangerous(command); !dangerous {
			t.Errorf("missed %q", command)
		}
	}
	for _, command := range []string{"ls -la", "go test ./...", "rm -rf ./build"} {
		if reason, dangerous := DetectDangerous(command); dangerous {
			t.Errorf("false positive %q: %s", command, reason)
		}
	}
}
