// Package assetserver provides a file server for web assets.
package assetserver

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"math/big"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A Server serves HTTP requests with the contents of a file system.
//
// The response headers are set appropriately for web assets so that they are
// cached effectively:
//
//   - The ETag header is set with a hash of the file contents
//   - The Cache-Control header is set to "public, max-age=60"
//
// If the requested file is tagged (see [Server.Tag]), the tag is removed before
// the file is retrieved. Tagged files are served with a Cache-Control header of
//
//	public, max-age=31536000, immutable
//
// If the Server was created with NewNoCache, all assets are instead served
// with Cache-Control: no-cache.
//
// In the following cases, Server sends a 404 Not Found response:
//
//   - If the requested file doesn't exist in the file system
//   - If the requested file is a directory
//   - If the requested name is tagged but the tag does not match the
//     corresponding file
//
// For other errors, Server sends a 500 Internal Server Error response.
type Server struct {
	fsys    fs.FS
	noCache bool

	// An rwmutex seems appropriate here: once we've loaded all the assets,
	// we never lock the mutex again.
	mu    sync.RWMutex
	cache map[string]*atomic.Pointer[fileInfo]
}

type fileInfo struct {
	// We assume the file is unchanged if the mtime+size are the same.
	mtime int64 // as unix nano
	size  int64

	tag         string
	contentType string
}

// New creates a Server from a file system.
//
// The files that are opened from the file system must implement [io.Seeker].
// The [fs.FS] implementations which satisfy this requirement include [embed.FS]
// and the result of calling [os.DirFS].
func New(fsys fs.FS) *Server {
	return &Server{
		fsys:  fsys,
		cache: make(map[string]*atomic.Pointer[fileInfo]),
	}
}

// NewNoCache is like New, but the returned Server serves all assets with
// Cache-Control: no-cache.
//
// NewNoCache is intended for non-production settings (such as local development).
func NewNoCache(fsys fs.FS) *Server {
	s := New(fsys)
	s.noCache = true
	return s
}

// Tag modifies the provided file name to include an asset tag preceding the
// first dot. The tag is based on a hash of the file contents.
// File names are slash-separated paths as given to the underlying [fs.FS].
// If the name starts with a slash, it is removed before retrieving the file.
func (s *Server) Tag(name string) (string, error) {
	var hadSlash bool
	name, hadSlash = strings.CutPrefix(name, "/")
	// Happy path: only call stat.
	info, err := s.tryCachedInfo(name)
	if err != nil {
		if err != errNoInfo {
			return "", err
		}
		// No cached info (or it's out of date). Recompute.
		var f seekerFile
		f, info, err = s.openWithInfo(name)
		if err != nil {
			return "", err
		}
		f.Close()
	}
	dir, base := path.Split(name)
	// We place the tag before the first dot (rather than before the last
	// dot) because files may have multiple extensions: "x.tar.gz",
	// "lib.min.js", etc.
	if head, tail, ok := strings.Cut(base, "."); ok {
		// head.xxxxxxxxxxxxx.tail
		base = head + "." + info.tag + "." + tail
	} else {
		// head.xxxxxxxxxxxxx
		base += "." + info.tag
	}
	tagged := path.Join(dir, base)
	if hadSlash {
		tagged = "/" + tagged
	}
	return tagged, nil
}

// removeTag looks for an asset tag as part of a file name and returns the tag
// and the equivalent name with the tag removed.
// If the name doesn't include a tag, removeTag returns "", s.
func removeTag(s string) (tag, name string) {
	dir, base := path.Split(s)
	head, tail, ok := strings.Cut(base, ".")
	if !ok {
		return "", s
	}
	if isTag(tail) {
		// head.xxxxxxxxxxxxxxxx
		return tail, path.Join(dir, head)
	}
	tag, tail, ok = strings.Cut(tail, ".")
	if !ok {
		return "", s
	}
	if isTag(tag) {
		// head.xxxxxxxxxxxxxxxx.tail
		return tag, path.Join(dir, head+"."+tail)
	}
	return "", s
}

// To make the tag strings as short as possible but still easy to read (no
// special characters), use a base62 encoding with alphabet {0-9, a-z, A-Z}.
// We'll generate 10 characters of output for ~60 bits of total output size.

const tagLen = 10

var (
	sixtyTwo   = big.NewInt(62)
	alphabet62 string
	inAlphabet [256]bool
)

func init() {
	var alph []byte
	for b := byte('0'); b <= '9'; b++ {
		alph = append(alph, b)
	}
	for b := byte('a'); b <= 'z'; b++ {
		alph = append(alph, b)
	}
	for b := byte('A'); b <= 'Z'; b++ {
		alph = append(alph, b)
	}
	if len(alph) != 62 {
		panic("bad alphabet")
	}
	for _, b := range alph {
		inAlphabet[b] = true
	}
	alphabet62 = string(alph)
}

func makeTag(b []byte) string {
	n := new(big.Int).SetBytes(b[:8]) // need ~60 bits
	m := new(big.Int)
	out := make([]byte, tagLen)
	for i := range out {
		n.DivMod(n, sixtyTwo, m)
		out[i] = alphabet62[m.Int64()]
	}
	return string(out)
}

func isTag(s string) bool {
	if len(s) != tagLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !inAlphabet[s[i]] {
			return false
		}
	}
	return true
}

type seekerFile interface {
	fs.File
	io.Seeker
}

var errNoInfo = errors.New("cached info for file is out of date or nonexistent")

// tryCachedInfo returns the cached info for the named file if it matches the
// contents of the file as gauged by the size and mtime.
// Otherwise it returns errNoInfo.
func (s *Server) tryCachedInfo(name string) (*fileInfo, error) {
	fi, err := fs.Stat(s.fsys, name)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fs.ErrNotExist
	}
	s.mu.RLock()
	p, ok := s.cache[name]
	s.mu.RUnlock()
	if !ok {
		return nil, errNoInfo
	}
	info := p.Load()
	if info == nil || fi.Size() != info.size || fi.ModTime().UnixNano() != info.mtime {
		return nil, errNoInfo
	}
	return info, nil
}

// openWithInfo opens the named file and also retrieves its fileInfo summary,
// from cache if possible.
// The info matches the contents of the file, as gauged by the size and mtime,
// unless the file is changing as it is being read (in which case all bets are
// off).
func (s *Server) openWithInfo(name string) (f seekerFile, info *fileInfo, err error) {
	fv, err := s.fsys.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			fv.Close()
		}
	}()
	fi, err := fv.Stat()
	if err != nil {
		return nil, nil, err
	}
	if fi.IsDir() {
		return nil, nil, fs.ErrNotExist
	}
	f = fv.(seekerFile)
	s.mu.RLock()
	p, ok := s.cache[name]
	s.mu.RUnlock()
	if !ok {
		s.mu.Lock()
		p, ok = s.cache[name]
		if !ok {
			p = new(atomic.Pointer[fileInfo])
			s.cache[name] = p
		}
		s.mu.Unlock()
	}

	info = p.Load()
	if info != nil && fi.Size() == info.size && fi.ModTime().UnixNano() == info.mtime {
		return f, info, nil
	}

	// The info doesn't match. Reload it from the file and then store it in
	// the cache.
	info, err = s.readInfo(f)
	if err != nil {
		return nil, nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	p.Store(info)
	return f, info, nil
}

func (s *Server) readInfo(f seekerFile) (*fileInfo, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fi := &fileInfo{
		mtime: stat.ModTime().UnixNano(),
		size:  stat.Size(),
	}

	h := sha256.New()
	fi.contentType = mime.TypeByExtension(path.Ext(stat.Name()))
	if fi.contentType != "" {
		if _, err := io.Copy(h, f); err != nil {
			return nil, err
		}
	} else {
		var sniffBuf bytes.Buffer
		// http.DetectContentType uses at most 512 bytes.
		_, err := io.CopyN(io.MultiWriter(h, &sniffBuf), f, 512)
		switch err {
		case nil:
			// There's more data to hash.
			if _, err := io.Copy(h, f); err != nil {
				return nil, err
			}
		case io.EOF:
			// Read the whole file.
		default:
			return nil, err
		}
		fi.contentType = http.DetectContentType(sniffBuf.Bytes())
	}
	fi.tag = makeTag(h.Sum(nil))
	return fi, nil
}

// ServeHTTP serves file system contents matching the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET,HEAD")
		http.Error(w, "405 Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	pth := r.URL.Path
	if !strings.HasPrefix(pth, "/") {
		pth = "/" + pth
		r.URL.Path = pth
	}
	pth = path.Clean(pth)

	if pth == "/" {
		http.NotFound(w, r)
		return
	}

	tag, taglessPath := removeTag(pth)
	f, info, err := s.openWithInfo(taglessPath[1:]) // trim leading /
	if err != nil {
		writeFSError(w, r, err)
		return
	}
	defer f.Close()
	// If the tag is wrong/outdated, 404.
	if tag != "" && tag != info.tag {
		http.NotFound(w, r)
		return
	}

	// Redirect trailing slashes to no-slash paths.
	if strings.HasSuffix(r.URL.Path, "/") {
		// We cannot use http.Redirect because it changes the path to be
		// absolute and that doesn't work if we're running under http.StripPrefix.
		target := "../" + path.Base(pth)
		if q := r.URL.RawQuery; q != "" {
			target += "?" + q
		}
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}

	h := w.Header()
	var cc string
	if s.noCache {
		cc = "no-cache"
	} else {
		if tag == "" {
			cc = "public, max-age=60"
		} else {
			cc = "public, max-age=31536000, immutable"
		}
	}
	h.Set("Cache-Control", cc)
	h.Set("ETag", `"`+info.tag+`"`)
	// Only set Content-Type if it wasn't set by the caller.
	if _, ok := h["Content-Type"]; !ok {
		if info.contentType != "" {
			h.Set("Content-Type", info.contentType)
		} else {
			h["Content-Type"] = nil // prevent ServeContent from sniffing
		}
	}

	http.ServeContent(w, r, pth, time.Unix(0, info.mtime), f)
}

func writeFSError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	// Don't turn permission errors into 403s here like FileServer does.
	// That generally isn't helpful in this domain and it leaks information
	// about a misconfiguration in the system.
	http.Error(w, "500 Internal Server Error", 500)
}
