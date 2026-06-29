package base

import "testing"

// TestParseChecksum pins the two checksums-file shapes parseChecksum accepts
// (a sha256sum listing matched by basename, and a lone per-asset digest) plus
// the rejections: a missing entry and a digest for a different file.
func TestParseChecksum(t *testing.T) {
	const a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	cases := []struct {
		name string
		body string
		want string // "" means an error is expected
	}{
		{"two-space listing", a + "  tool-linux.tar\n" + b + "  other.tar\n", a},
		{"binary-mode star", a + " *tool-linux.tar\n", a},
		{"basename match against a path", a + "  dist/tool-linux.tar\n", a},
		{"lone digest applies to any name", a + "\n", a},
		{"no entry for the asset", b + "  other.tar\n", ""},
		{"garbage", "not a checksums file\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseChecksum([]byte(c.body), "tool-linux.tar")
			if c.want == "" {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestIsHex64(t *testing.T) {
	const ok = "0123456789abcdefABCDEF000000000000000000000000000000000000000000"
	if !isHex64(ok) {
		t.Fatalf("%q should be valid", ok)
	}
	for _, bad := range []string{"", "abc", ok + "0", ok[:63], ok[:63] + "g"} {
		if isHex64(bad) {
			t.Fatalf("%q should be invalid", bad)
		}
	}
}
