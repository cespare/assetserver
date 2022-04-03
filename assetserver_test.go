package assetserver

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"testing"

	"github.com/cespare/webtest"
)

func TestTag(t *testing.T) {
	s := New(os.DirFS("testdata/assets"))
	for _, tt := range []struct {
		name string
		want string
	}{
		{"d/style.css", "d/style." + tag("style\n") + ".css"},
		{"a.js", "a." + tag("ajs\n") + ".js"},
		{"d/sub/noext", "d/sub/noext." + tag("<!doctype html>\n")},
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
		{"a.1231231234.js", "1231231234", "a.js"},
		{"d/sub/noext.xyzXYZxyzX", "xyzXYZxyzX", "d/sub/noext"},

		{"d/style.abcABC12345.css", "", "d/style.abcABC12345.css"},
		{"a.12312312.js", "", "a.12312312.js"},
		{"d/sub/noext.xyzXYZ_xyz", "", "d/sub/noext.xyzXYZ_xyz"},
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

func TestServeHTTP(t *testing.T) {
	s := New(os.DirFS("testdata/assets"))
	webtest.TestHandler(t, "testdata/servehttp.txt", s)
}

//go:embed testdata/assets
var embedFS embed.FS

// Check that nothing about the embed.FS implementation of fs.FS is different
// in a way that causes problems.
// (In particular, verify that it supports seeking.)
func TestServeHTTPEmbed(t *testing.T) {
	fsys, err := fs.Sub(embedFS, "testdata/assets")
	if err != nil {
		t.Fatal(err)
	}
	s := New(fsys)
	webtest.TestHandler(t, "testdata/servehttp.txt", s)
}

func TestPrintHashes(t *testing.T) {
	t.Skip("un-skip this test to print out all testdata asset hashes")
	fsys := os.DirFS("testdata/assets")
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		fmt.Printf("%s\t%s\n", p, tag(string(b)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
