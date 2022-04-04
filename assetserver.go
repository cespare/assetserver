// Package assetserver provides a file server for web assets.
//
// FIXME: Write more. Document restrictions on underlying fs.
package assetserver

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
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

// A Server serves web asset files from a file system.
type Server struct {
	fsys fs.FS

	// An rwmutex seems appropriate here: once we've loaded all the assets,
	// we never lock the mutex again.
	mu    sync.RWMutex
	cache map[string]*atomic.Value // of fileInfo
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
// The files that are opened from the file system must implement io.Seeker.
func New(fsys fs.FS) *Server {
	return &Server{
		fsys:  fsys,
		cache: make(map[string]*atomic.Value),
	}
}

// Tag modifies the provided file name to include an asset tag.
// The tag is based on a hash of the file contents.
// File names are slash-separated paths as given to the underlying fs.FS.
func (s *Server) Tag(name string) (string, error) {
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
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		// name.xxxxxxxxxxxxx.ext
		base = base[:i] + "." + info.tag + base[i:]
	} else {
		// name.xxxxxxxxxxxxx
		base += "." + info.tag
	}
	return path.Join(dir, base), nil
}

// removeTag looks for an asset tag as part of a file name and returns the tag
// and the equivalent name with the tag removed.
// If the name doesn't include a tag, removeTag returns "", s.
func removeTag(s string) (tag, name string) {
	dir, base := path.Split(s)
	j := strings.LastIndexByte(base, '.')
	if j < 0 {
		return "", s
	}
	if tag := base[j+1:]; isTag(tag) {
		// name.xxxxxxxxxxxxxxxx
		return tag, path.Join(dir, base[:j])
	}
	i := strings.LastIndexByte(base[:j], '.')
	if i < 0 {
		return "", s
	}
	if tag := base[i+1 : j]; isTag(tag) {
		// name.xxxxxxxxxxxxxxxx.ext
		return tag, path.Join(dir, base[:i]+base[j:])
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

func (s *Server) open(name string) (seekerFile, error) {
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	return f.(seekerFile), nil
}

var errNoInfo = errors.New("cached info for file is out of date or nonexistent")

// tryCachedInfo returns the cached info for the named file if it matches the
// contents of the file as gauged by the size and mtime.
// Otherwise it returns errNoInfo.
func (s *Server) tryCachedInfo(name string) (fileInfo, error) {
	fi, err := fs.Stat(s.fsys, name)
	if err != nil {
		return fileInfo{}, err
	}
	s.mu.RLock()
	v, ok := s.cache[name]
	s.mu.RUnlock()
	if !ok {
		return fileInfo{}, errNoInfo
	}
	info := v.Load().(fileInfo)
	if fi.Size() != info.size || fi.ModTime().UnixNano() != info.mtime {
		return fileInfo{}, errNoInfo
	}
	return info, nil
}

// openWithInfo opens the named file and also retrieves its fileInfo summar,
// from cache if possible.
// The info matches the contents of the file as gauged by the size and mtime,
// unless the file is changing as its being read (in which case all bets are off).
func (s *Server) openWithInfo(name string) (f seekerFile, info fileInfo, err error) {
	fv, err := s.fsys.Open(name)
	if err != nil {
		return nil, info, err
	}
	f = fv.(seekerFile)
	defer func() {
		if err != nil {
			f.Close()
		}
	}()
	fi, err := f.Stat()
	if err != nil {
		return nil, info, err
	}
	s.mu.RLock()
	v, ok := s.cache[name]
	s.mu.RUnlock()
	if !ok {
		s.mu.Lock()
		v, ok = s.cache[name]
		if !ok {
			v = new(atomic.Value)
			v.Store(fileInfo{})
			s.cache[name] = v
		}
		s.mu.Unlock()
	}

	info = v.Load().(fileInfo)
	if fi.Size() == info.size && fi.ModTime().UnixNano() == info.mtime {
		return f, info, nil
	}

	// The info doesn't match. Reload it from the file and then store it in
	// the cache.
	info, err = s.readInfo(f)
	if err != nil {
		return nil, info, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, info, err
	}
	v.Store(info)
	return f, info, nil
}

func (s *Server) readInfo(f seekerFile) (fileInfo, error) {
	var fi fileInfo
	stat, err := f.Stat()
	if err != nil {
		return fi, err
	}
	fi.mtime = stat.ModTime().UnixNano()
	fi.size = stat.Size()

	h := sha256.New()
	fi.contentType = mime.TypeByExtension(path.Ext(stat.Name()))
	if fi.contentType != "" {
		if _, err := io.Copy(h, f); err != nil {
			return fi, err
		}
	} else {
		var sniffBuf bytes.Buffer
		// http.DetectContentType uses at most 512 bytes.
		_, err := io.CopyN(io.MultiWriter(h, &sniffBuf), f, 512)
		switch err {
		case nil:
			// There's more data to hash.
			if _, err := io.Copy(h, f); err != nil {
				return fi, err
			}
		case io.EOF:
			// Read the whole file.
		default:
			return fi, err
		}
		fi.contentType = http.DetectContentType(sniffBuf.Bytes())
	}
	fi.tag = makeTag(h.Sum(nil))
	return fi, nil
}

// FIXME: doc
// Requests for the tagged file are resolved to the original name
// and served with a long-term max-age in the Cache-Control header.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET,HEAD")
		http.Error(w, "405 Method Not Allowed", 405)
		return
	}
	name := r.URL.Path
	if name == "/" || !strings.HasPrefix(name, "/") {
		http.NotFound(w, r)
		return
	}
	name = path.Clean(name)[1:]

	var tag string
	tag, name = removeTag(name)

	f, info, err := s.openWithInfo(name)
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

	var maxAge time.Duration
	if tag == "" {
		maxAge = 10 * time.Minute
	} else {
		maxAge = 365 * 24 * time.Hour
	}
	h := w.Header()
	h.Set("Cache-Control", fmt.Sprintf("max-age=%d", int64(maxAge.Seconds())))
	h.Set("ETag", `"`+info.tag+`"`)
	// Only set Content-Type if it wasn't set by the caller.
	if _, ok := h["Content-Type"]; !ok {
		if info.contentType != "" {
			h.Set("Content-Type", info.contentType)
		} else {
			h["Content-Type"] = nil // prevent ServeContent from sniffing
		}
	}
	http.ServeContent(w, r, name, time.Unix(0, info.mtime), f)
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
