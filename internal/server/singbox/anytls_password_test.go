package singbox

import (
	"strings"
	"testing"
)

// The password is machine-generated (§10.1 实现修订 2026-09-16), so its shape is a
// contract rather than a preference: every character has to survive a JSON
// field, a URI auth component and a Clash YAML scalar without escaping.
func TestGenerateAnytlsPasswordShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		pw, err := GenerateAnytlsPassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != AnytlsPasswordLen {
			t.Fatalf("password %q has %d chars, want %d", pw, len(pw), AnytlsPasswordLen)
		}
		for _, r := range pw {
			if !strings.ContainsRune(anytlsPasswordCharset, r) {
				t.Fatalf("password %q contains %q, outside the charset", pw, r)
			}
		}
		if seen[pw] {
			t.Fatalf("duplicate password %q in 500 draws", pw)
		}
		seen[pw] = true
	}
}
