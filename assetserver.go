package assetserver

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/tv42/zbase32"
)

type Server struct {
	fsys fs.FS

	mu sync.Mutex
	// TODO: Use generic sync.Map when that exists.
	cache map[string]*fileInfo // nil in dev mode
}

type fileInfo struct {
	hash        string
	contentType string
}

func NewProd(fsys fs.FS) *Server {
	return &Server{
		fsys:  fsys,
		cache: make(map[string]*fileInfo),
	}
}

func NewDev(fsys fs.FS) *Server {
	return &Server{
		fsys: fsys,
	}
}

func (s *Server) HashName(name string) (string, error) {
	info, err := s.infoCached(name)
	if err != nil {
		return "", err
	}
	dir, base := path.Split(name)
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		// name.xxxxxxxxxxxxx.ext
		base = name[:i] + "." + info.hash + name[i:]
	} else {
		// name.xxxxxxxxxxxxx
		base = name + "." + info.hash
	}
	return path.Join(dir, base), nil
}

func removeNameHash(s string) (h, name string) {
	dir, base := path.Split(s)
	j := strings.LastIndexByte(base, '.')
	if j < 0 {
		return "", s
	}
	if h := base[j+1:]; isHash(h) {
		// name.xxxxxxxxxxxxxxxx
		return h, path.Join(dir, base[:j])
	}
	i := strings.LastIndexByte(base[:j], '.')
	if i < 0 {
		return "", s
	}
	if h := base[i+1 : j]; isHash(h) {
		// name.xxxxxxxxxxxxxxxx.ext
		return h, path.Join(dir, base[:i]+base[j:])
	}
	return "", s
}

const hashBytes = 8

var (
	hashLen      = zbase32.EncodedLen(hashBytes)
	zbase32Bytes [256]bool
)

func init() {
	const zbase32Alphabet = "ybndrfg8ejkmcpqxot1uwisza345h769"
	for i := 0; i < len(zbase32Alphabet); i++ {
		zbase32Bytes[zbase32Alphabet[i]] = true
	}
}

func isHash(s string) bool {
	if len(s) != hashLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !zbase32Bytes[s[i]] {
			return false
		}
	}
	return true
}

func (s *Server) infoCached(name string) (*fileInfo, error) {
	s.mu.Lock()
	info, ok := s.cache[name]
	s.mu.Unlock()
	if ok {
		return info, nil
	}
	// TODO: We could single-flight this.
	h, err := s.hash(name)
	if err != nil {
		return nil, err
	}
	ct, err := s.sniffContentType(name)
	if err != nil {
		return nil, err
	}
	info = &fileInfo{hash: h, contentType: ct}
	if s.cache != nil {
		s.mu.Lock()
		s.cache[name] = info
		s.mu.Unlock()
	}
	return info, nil
}

func (s *Server) hash(name string) (string, error) {
	f, err := s.fsys.Open(name)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return zbase32.EncodeToString(h.Sum(nil)[:hashBytes]), nil
}

func (s *Server) sniffContentType(name string) (string, error) {
	if ctype := mime.TypeByExtension(path.Ext(name)); ctype != "" {
		return ctype, nil
	}
	f, err := s.fsys.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf [512]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil {
		return "", err
	}
	return http.DetectContentType(buf[:n]), nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET,HEAD")
		http.Error(w, "405 Method Not Allowed", 405)
		return
	}
	name := r.URL.Path
	if !strings.HasPrefix(name, "/") {
		http.NotFound(w, r)
		return
	}
	name = path.Clean(name)[1:]

	var reqHash string
	reqHash, name = removeNameHash(name)
	info, err := s.infoCached(name)
	if err != nil {
		writeFSError(w, r, err)
		return
	}
	if reqHash != info.hash {
		http.NotFound(w, r)
		return
	}

	f, err := s.fsys.Open(name)
	if err != nil {
		writeFSError(w, r, err)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		writeFSError(w, r, err)
		return
	}
	if stat.IsDir() {
		// Don't expose any information about directories.
		http.NotFound(w, r)
		return
	}

	content, ok := f.(io.ReadSeeker)
	if !ok {
		// TODO: explain hacks
		content = &sizeReadSeeker{r: f, size: stat.Size()}
	}
	var maxAge time.Duration
	if reqHash == "" {
		maxAge = 10 * time.Minute
	} else {
		maxAge = 365 * 24 * time.Hour
	}
	h := w.Header()
	h.Set("Cache-Control", fmt.Sprintf("max-age=%d", int64(maxAge.Seconds())))
	h.Set("ETag", `"`+info.hash+`"`)
	if info.contentType != "" {
		h.Set("Content-Type", info.contentType)
	} else {
		h["Content-Type"] = nil // prevent ServeContent from sniffing
	}
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), content)
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

func fakeReadSeeker(f fs.File) (io.ReadSeeker, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return &sizeReadSeeker{r: f, size: stat.Size()}, nil
}

type sizeReadSeeker struct {
	r    io.Reader
	size int64 // total file size

	n   int64 // how many bytes have been read
	end bool  // did we "seek" to the end?
}

func (r *sizeReadSeeker) Read(b []byte) (int, error) {
	if r.end {
		return 0, io.EOF
	}
	n, err := r.r.Read(b)
	r.n += int64(n)
	return n, err
}

var errSeek = errors.New("unsupported seek operation")

func (r *sizeReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if offset != 0 {
		return 0, errSeek
	}
	// No seeking after reading has started.
	if r.n > 0 {
		return 0, errSeek
	}
	switch whence {
	case io.SeekStart:
		r.end = false
		return 0, nil
	case io.SeekCurrent:
		return 0, nil
	case io.SeekEnd:
		r.end = true
		return r.size, nil
	default:
		return 0, errors.New("bad whence value for seek")
	}
}
