// FIXME: doc
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

// FIXME: doc
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

// FIXME: doc
func New(fsys fs.FS) *Server {
	return &Server{
		fsys:  fsys,
		cache: make(map[string]*atomic.Value),
	}
}

func (s *Server) open(name string) (seekerFile, error) {
	f, err := s.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	return f.(seekerFile), nil
}

// FIXME: doc
func (s *Server) Tag(name string) (string, error) {
	// TODO: Add a new happy path where we only call stat, not open.
	f, err := s.open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := s.getInfo(name, f)
	if err != nil {
		return "", err
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

// FIXME: doc
func removeTag(s string) (h, name string) {
	dir, base := path.Split(s)
	j := strings.LastIndexByte(base, '.')
	if j < 0 {
		return "", s
	}
	if h := base[j+1:]; isTag(h) {
		// name.xxxxxxxxxxxxxxxx
		return h, path.Join(dir, base[:j])
	}
	i := strings.LastIndexByte(base[:j], '.')
	if i < 0 {
		return "", s
	}
	if h := base[i+1 : j]; isTag(h) {
		// name.xxxxxxxxxxxxxxxx.ext
		return h, path.Join(dir, base[:i]+base[j:])
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

// getInfo retrieves the fileInfo summary of f, from cache if possible.
// The info matches the contents of f as gauged by the size and mtime,
// unless the file is changing as its being read
// (in which case all bets are off).
func (s *Server) getInfo(name string, f seekerFile) (fileInfo, error) {
	fi, err := f.Stat()
	if err != nil {
		return fileInfo{}, err
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

	info := v.Load().(fileInfo)
	if fi.Size() == info.size && fi.ModTime().UnixNano() == info.mtime {
		return info, nil
	}

	// The info doesn't match. Reload it from the file and then store it in
	// the cache.
	info, err = s.readInfo(f)
	if err != nil {
		return fileInfo{}, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fileInfo{}, err
	}
	v.Store(info)
	return info, nil
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

	f, err := s.open(name)
	if err != nil {
		writeFSError(w, r, err)
		return
	}
	defer f.Close()
	info, err := s.getInfo(name, f)
	if err != nil {
		writeFSError(w, r, err)
		return
	}
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
