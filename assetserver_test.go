package assetserver

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/fstest"

	"github.com/cespare/webtest"
	"github.com/google/go-cmp/cmp"
)

func TestTag(t *testing.T) {
	s := New(os.DirFS("testdata/assets"))
	for _, tt := range []struct {
		name string
		want string
	}{
		{"d/style.css", "d/style." + hashTag("style\n") + ".css"},
		{"a.js", "a." + hashTag("ajs\n") + ".js"},
		{"d/sub/noext", "d/sub/noext." + hashTag("<!doctype html>\n")},
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

func hashTag(text string) string {
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

// FIXME: test for files that are changing
// FIXME: test dir requests

// Exercise content sniffing and hashing on files larger than 512 bytes.
func TestServeLargeFiles(t *testing.T) {
	content := make([]byte, 100e3)
	for i := range content {
		// Include 0 bytes to be detected as a binary file type.
		content[i] = byte(i)
	}
	tag := hashTag(string(content))
	fsys := fstest.MapFS{
		"d/f": &fstest.MapFile{Data: content},
	}
	s := New(fsys)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/d/f", nil)
	s.ServeHTTP(w, req)
	resp := w.Result()
	checkResponseCode(t, resp, 200)
	checkResponseBody(t, resp, content)
	checkResponseHeader(t, resp, "ETag", `"`+tag+`"`)
}

func checkResponseCode(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("got response status %d; want %d", resp.StatusCode, want)
	}
}

func checkResponseBody(t *testing.T, resp *http.Response, want []byte) {
	t.Helper()
	got, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		// Shouldn't happen due to ResponseRecorder guarantees.
		t.Fatalf("error reading response body: %s", err)
	}
	if diff := cmp.Diff(got, want); diff != "" {
		t.Fatalf("wrong response body (-got, +want):\n%s", diff)
	}
}

func checkResponseHeader(t *testing.T, resp *http.Response, header, want string) {
	t.Helper()
	got := resp.Header.Get(header)
	if got != want {
		t.Fatalf("wrong value for header %q: want %q; got %q", header, got, want)
	}
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
		fmt.Printf("%s\t%s\n", p, hashTag(string(b)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
