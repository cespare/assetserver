package assetserver

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"
)

func TestCacheControlOverride(t *testing.T) {
	t.Setenv("GODEBUG", "httpservecontentkeepheaders=0")
	const body = "asset\n"
	tag := hashTag(body)
	tagged := "/a." + tag + ".txt"
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fsys := fstest.MapFS{
		"a.txt":     {Data: []byte(body), ModTime: mtime},
		"dir/b.txt": {Data: []byte("b\n")},
	}

	tests := []struct {
		name   string
		path   string
		method string
		header http.Header
		code   int
		body   string
	}{
		{
			name: "untagged",
			path: "/a.txt",
			code: 200,
			body: body,
		},
		{
			name: "tagged",
			path: tagged,
			code: 200,
			body: body,
		},
		{
			name:   "head",
			path:   "/a.txt",
			method: "HEAD",
			code:   200,
		},
		{
			name:   "tagged-head",
			path:   tagged,
			method: "HEAD",
			code:   200,
		},
		{
			name:   "not-modified",
			path:   "/a.txt",
			header: http.Header{"If-None-Match": {`"` + tag + `"`}},
			code:   304,
		},
		{
			name:   "tagged-not-modified",
			path:   tagged,
			header: http.Header{"If-None-Match": {`"` + tag + `"`}},
			code:   304,
		},
		{
			name:   "weak-etag",
			path:   tagged,
			header: http.Header{"If-None-Match": {`W/"` + tag + `"`}},
			code:   304,
		},
		{
			name: "modified-since",
			path: "/a.txt",
			header: http.Header{
				"If-Modified-Since": {mtime.Format(http.TimeFormat)},
			},
			code: 304,
		},
		{
			name:   "range",
			path:   tagged,
			header: http.Header{"Range": {"bytes=0-1"}},
			code:   206,
			body:   "as",
		},
		{
			name:   "precondition-failed",
			path:   tagged,
			header: http.Header{"If-Match": {`"different"`}},
			code:   412,
		},
		{
			name:   "invalid-range",
			path:   "/a.txt",
			header: http.Header{"Range": {"invalid"}},
			code:   416,
		},
		{
			name:   "unsatisfiable-range",
			path:   tagged,
			header: http.Header{"Range": {"bytes=100-"}},
			code:   416,
		},
		{
			name: "redirect",
			path: "/a.txt/",
			code: 308,
		},
		{
			name: "tagged-redirect",
			path: tagged + "/",
			code: 308,
		},
		{
			name: "root",
			path: "/",
			code: 404,
		},
		{
			name: "missing",
			path: "/missing.txt",
			code: 404,
		},
		{
			name: "directory",
			path: "/dir",
			code: 404,
		},
		{
			name: "stale-tag",
			path: "/a." + hashTag("old") + ".txt",
			code: 404,
		},
		{
			name:   "method-not-allowed",
			path:   "/a.txt",
			method: "POST",
			code:   405,
		},
	}
	for _, mode := range []struct {
		name string
		new  func(fs.FS) *Server
	}{
		{"cache", New},
		{"no-cache", NewNoCache},
	} {
		s := mode.new(fsys)
		for _, policy := range []struct {
			name   string
			values []string
		}{
			{"private", []string{"private, no-cache"}},
			{"no-store", []string{"no-store"}},
			{"public", []string{"public, max-age=31536000, immutable"}},
			{"multiple-values", []string{"private", "no-cache"}},
			{"nil", nil},
			{"empty-slice", []string{}},
			{"empty-value", []string{""}},
		} {
			t.Run(mode.name+"/"+policy.name, func(t *testing.T) {
				h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header()["Cache-Control"] = policy.values
					s.ServeHTTP(w, r)
				})
				srv := httptest.NewServer(h)
				defer srv.Close()
				client := srv.Client()
				client.CheckRedirect = func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				}
				want := policy.values
				if len(want) == 0 {
					want = nil
				}
				for _, tt := range tests {
					t.Run(tt.name, func(t *testing.T) {
						r, err := http.NewRequest(tt.method, srv.URL+tt.path, nil)
						if err != nil {
							t.Fatal(err)
						}
						r.Header = tt.header
						resp, err := client.Do(r)
						if err != nil {
							t.Fatal(err)
						}
						defer resp.Body.Close()
						checkResponseCode(t, resp, tt.code)
						got := resp.Header.Values("Cache-Control")
						if diff := cmp.Diff(want, got); diff != "" {
							t.Fatalf("Cache-Control (-want, +got):\n%s", diff)
						}
						switch tt.code {
						case 200, 206, 304:
							checkResponseHeader(t, resp, "ETag", `"`+tag+`"`)
							checkResponseBody(t, resp, []byte(tt.body))
						}
					})
				}
			})
		}
	}
}

func TestCacheControlDefaultErrors(t *testing.T) {
	t.Setenv("GODEBUG", "httpservecontentkeepheaders=0")
	const body = "asset\n"
	fsys := fstest.MapFS{"a.txt": {Data: []byte(body)}}
	for _, tt := range []struct {
		name   string
		server *Server
		path   string
		policy string
	}{
		{
			name:   "untagged",
			server: New(fsys),
			path:   "/a.txt",
			policy: "public, max-age=60",
		},
		{
			name:   "tagged",
			server: New(fsys),
			path:   "/a." + hashTag(body) + ".txt",
			policy: "public, max-age=31536000, immutable",
		},
		{
			name:   "no-cache",
			server: NewNoCache(fsys),
			path:   "/a." + hashTag(body) + ".txt",
			policy: "no-cache",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tt.path, nil)
			r.Header.Set("Range", "bytes=100-")
			w := httptest.NewRecorder()
			tt.server.ServeHTTP(w, r)
			resp := w.Result()
			defer resp.Body.Close()
			checkResponseCode(t, resp, 416)

			// Default policies should retain ServeContent's error behavior,
			// which depends on the Go version.
			ref := httptest.NewRecorder()
			ref.Header().Set("Cache-Control", tt.policy)
			http.ServeContent(ref, r, "a.txt", time.Time{}, strings.NewReader(body))
			want := ref.Result()
			defer want.Body.Close()
			checkResponseHeader(
				t,
				resp,
				"Cache-Control",
				want.Header.Get("Cache-Control"),
			)
		})
	}
}

func TestCacheControlSeekError(t *testing.T) {
	t.Setenv("GODEBUG", "httpservecontentkeepheaders=0")
	fsys := seekErrorFS{fstest.MapFS{"a.txt": {Data: []byte("asset\n")}}}
	s := New(fsys)
	w := httptest.NewRecorder()
	w.Header().Set("Cache-Control", "no-store")
	s.ServeHTTP(w, httptest.NewRequest("GET", "/a.txt", nil))
	resp := w.Result()
	defer resp.Body.Close()
	checkResponseCode(t, resp, 500)
	checkResponseHeader(t, resp, "Cache-Control", "no-store")
}

type seekErrorFS struct{ fs.FS }

func (f seekErrorFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return seekErrorFile{file.(seekerFile)}, nil
}

type seekErrorFile struct{ seekerFile }

func (f seekErrorFile) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 0, fs.ErrInvalid
	}
	return f.seekerFile.Seek(offset, whence)
}

func TestCacheControlReadFrom(t *testing.T) {
	const body = "asset\n"
	s := New(fstest.MapFS{"a.txt": {Data: []byte(body)}})
	w := &readFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	w.Header().Set("Cache-Control", "no-store")
	s.ServeHTTP(w, httptest.NewRequest("GET", "/a.txt", nil))
	if !w.readFrom {
		t.Fatal("did not call the underlying writer's ReadFrom method")
	}
	resp := w.Result()
	defer resp.Body.Close()
	checkResponseCode(t, resp, 200)
	checkResponseHeader(t, resp, "Cache-Control", "no-store")
	checkResponseBody(t, resp, []byte(body))
}

type readFromRecorder struct {
	*httptest.ResponseRecorder
	readFrom bool
}

func (w *readFromRecorder) ReadFrom(r io.Reader) (int64, error) {
	w.readFrom = true
	return io.Copy(w.ResponseRecorder, r)
}
