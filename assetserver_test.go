package assetserver

import (
	"crypto/sha256"
	"os"
	"testing"
)

func TestTag(t *testing.T) {
	s := New(os.DirFS("testdata"))
	for _, tt := range []struct {
		name string
		want string
	}{
		{"d/style.css", "d/style." + tag("style\n") + ".css"},
		{"a.js", "a." + tag("ajs\n") + ".js"},
		{"d/sub/noext", "d/sub/noext." + tag("nox\n")},
	} {
		got, err := s.Tag(tt.name)
		if err != nil {
			t.Fatalf("Tag(%q): %s", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("Tag(%q): got %q; want %q", tt.name, got, tt.want)
		}
	}
}

func TestRemoveTag(t *testing.T) {
	for _, tt := range []struct {
		s    string
		h    string
		name string
	}{
		{"d/style.abcABC1234.css", "abcABC1234", "d/style.css"},
	} {
		h, name := removeTag(tt.s)
		if h != tt.h || name != tt.name {
			t.Errorf("removeTag(%q): got (%q, %q); want (%q, %q)",
				tt.s, h, name, tt.h, tt.name)
		}
	}
}

func tag(text string) string {
	b := sha256.Sum256([]byte(text))
	return makeTag(b[:])
}
