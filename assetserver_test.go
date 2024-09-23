package assetserver

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/cespare/webtest"
	"github.com/google/go-cmp/cmp"
	"github.com/google/renameio"
	"golang.org/x/sync/errgroup"
)

func TestTag(t *testing.T) {
	s := New(os.DirFS("testdata/assets"))
	for _, tt := range []struct {
		name string
		want string
	}{
		{"d/style.css", "d/style." + hashTag("style\n") + ".css"},
		{"/d/style.css", "/d/style." + hashTag("style\n") + ".css"},
		{"a.js", "a." + hashTag("ajs\n") + ".js"},
		{"b.min.js", "b." + hashTag("b\n") + ".min.js"},
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
		{"/d/style.abcABC1234.css", "abcABC1234", "/d/style.css"},
		{"a.1231231234.js", "1231231234", "a.js"},
		{"b.1231231234.min.js", "1231231234", "b.min.js"},
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

func TestServeHTTPNoCache(t *testing.T) {
	s := NewNoCache(os.DirFS("testdata/assets"))
	webtest.TestHandler(t, "testdata/nocache.txt", s)
}

// The interaction of redirects and http.StripPrefix is a bit subtle, so test it
// explicitly.
func TestStripPrefix(t *testing.T) {
	for _, p := range []struct {
		name   string
		prefix string
	}{
		{"no-trailing-slash", "/sub"},
		{"trailing-slash", "/sub/"},
	} {
		t.Run(p.name, func(t *testing.T) {
			h := http.StripPrefix(p.prefix, New(os.DirFS("testdata/assets")))
			// Use a real server+client to test redirects.
			s := httptest.NewServer(h)
			t.Cleanup(s.Close)
			for _, tt := range []struct {
				pth  string
				want string
			}{
				{"/sub/d/style.css", "style\n"},
				{"/sub/d/style.css/", "style\n"},
				{"/sub/xyz/../a.js/", "ajs\n"},
			} {
				t.Run(tt.pth, func(t *testing.T) {
					resp, err := http.Get(s.URL + tt.pth)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					checkResponseCode(t, resp, 200)
					checkResponseBody(t, resp, []byte(tt.want))
				})
			}
		})
	}
}

// This tests the case that files are changing and being requested by multiple
// callers in parallel (so it's a good target for the race detector).
func TestChangingFiles(t *testing.T) {
	dir := t.TempDir()

	// Create 10 files: a.txt, b.txt, ..., j.txt.
	// Each file contains the letter of the file (a, b, ..., j)
	// and an incrementing sequence number starting at 0.
	writeFile := func(letter rune, seq int) {
		t.Helper()
		name := filepath.Join(dir, string(letter)+".txt")
		text := fmt.Sprintf("%c %d\n", letter, seq)
		if err := renameio.WriteFile(name, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for letter := 'a'; letter <= 'j'; letter++ {
		writeFile(letter, 0)
	}
	s := New(os.DirFS(dir))
	server := httptest.NewServer(s)
	defer server.Close()

	fetch := func(letter rune) (seq int, err error) {
		url := fmt.Sprintf("%s/%c.txt", server.URL, letter)
		resp, err := http.Get(url)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return 0, fmt.Errorf("non-200 response: %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return 0, fmt.Errorf("error reading response body: %s", err)
		}

		parts := strings.Fields(string(body))
		if len(parts) != 2 || parts[0] != string(letter) {
			return 0, fmt.Errorf("unexpected response %q for letter %c", body, letter)
		}
		seq, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, fmt.Errorf("bad sequence number %q for letter %c", parts[1], letter)
		}
		return seq, nil
	}

	start := make(chan struct{})
	eg, ctx := errgroup.WithContext(context.Background())
	for i := 0; i < 10; i++ {
		eg.Go(func() error {
			<-start
			lastSeq := make(map[rune]int)
			timer := time.NewTimer(0)
			<-timer.C
			defer timer.Stop()
			letter := 'a'
			for {
				seq, err := fetch(letter)
				if err != nil {
					return err
				}
				prev := lastSeq[letter]
				if seq < prev {
					return fmt.Errorf("for letter %c, went from seq=%d to %d", letter, prev, seq)
				}
				if letter == 'j' && seq == 10 {
					return nil
				}
				lastSeq[letter] = seq

				if letter == 'j' {
					letter = 'a'
				} else {
					letter++
				}

				if _, err := s.Tag(string(letter) + ".txt"); err != nil {
					return fmt.Errorf("error calling Tag for letter %c", letter)
				}

				timer.Reset(3 * time.Millisecond)
				select {
				case <-timer.C:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		})
	}
	close(start)

	eg.Go(func() error {
		timer := time.NewTimer(0)
		<-timer.C
		defer timer.Stop()
		for seq := 1; seq <= 10; seq++ {
			for letter := 'a'; letter <= 'j'; letter++ {
				writeFile(letter, seq)
				timer.Reset(time.Millisecond)
				select {
				case <-timer.C:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return nil
	})

	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}

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
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		// Shouldn't happen due to ResponseRecorder guarantees.
		t.Fatalf("error reading response body: %s", err)
	}
	if diff := cmp.Diff(string(got), string(want)); diff != "" {
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
