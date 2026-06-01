// Package searcher provides the core file-search engine.
package searcher

import (
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/zrep/zrep/internal/matcher"
)

// Result holds all matches for a single file.
type Result struct {
	Path    string
	Matches []matcher.LineMatch
	Count   int
	Err     error
}

// mmap files >= 32KB; use pooled reads for smaller ones to avoid syscall overhead.
const mmapThreshold = 32 * 1024
const maxMmapSize = 512 << 20

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1<<20)
		return &b
	},
}

// Searcher searches files using a Matcher.
type Searcher struct {
	m matcher.Matcher
}

// New creates a Searcher.
func New(m matcher.Matcher) *Searcher {
	return &Searcher{m: m}
}

// SearchFile searches a single file and returns its Result.
func (s *Searcher) SearchFile(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}

	var buf []byte
	var cleanup func()
	var fromMmap bool

	if fileSize >= mmapThreshold && fileSize <= maxMmapSize && runtime.GOOS != "windows" {
		b, fn, err := mmapFile(path)
		if err == nil {
			buf = b
			cleanup = fn
			fromMmap = true
		}
	}

	if !fromMmap {
		var err error
		buf, err = readFilePooled(path, fileSize)
		if err != nil {
			return Result{Path: path, Err: err}
		}
		defer func() {
			bp := &buf
			bufPool.Put(bp)
		}()
	}

	// Skip binary files: scan first 8KB for null bytes
	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if hasBinaryNull(probe) {
		if fromMmap {
			cleanup()
		}
		return Result{Path: path}
	}

	lm := make([]matcher.LineMatch, 0, 8)
	mb := make([]matcher.Match, 0, 8)
	lm, _ = matcher.FindMatchingLines(buf, s.m, 0, lm, mb)

	if fromMmap {
		// Copy line bytes before munmap; Line slices point into the mapping.
		for i := range lm {
			cp := make([]byte, len(lm[i].Line))
			copy(cp, lm[i].Line)
			lm[i].Line = cp
		}
		cleanup()
	}

	return Result{Path: path, Matches: lm}
}

// SearchFileInvert returns lines that do not match.
func (s *Searcher) SearchFileInvert(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if !fromMmap {
		defer func() {
			bp := &buf
			bufPool.Put(bp)
		}()
	}

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if hasBinaryNull(probe) {
		if cleanup != nil {
			cleanup()
		}
		return Result{Path: path}
	}

	lm := make([]matcher.LineMatch, 0, 8)
	lm = matcher.FindNonMatchingLines(buf, s.m, 0, lm)

	if fromMmap {
		for i := range lm {
			cp := make([]byte, len(lm[i].Line))
			copy(cp, lm[i].Line)
			lm[i].Line = cp
		}
		cleanup()
	}

	return Result{Path: path, Matches: lm}
}

// SearchFileCount counts matching lines without materializing line matches.
func (s *Searcher) SearchFileCount(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	if !fromMmap {
		defer func() {
			bp := &buf
			bufPool.Put(bp)
		}()
	}

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if hasBinaryNull(probe) {
		return Result{Path: path}
	}

	return Result{Path: path, Count: matcher.CountMatchingLines(buf, s.m)}
}

// SearchFileCountWords counts matching lines with ASCII whole-word boundaries.
func (s *Searcher) SearchFileCountWords(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	if !fromMmap {
		defer func() {
			bp := &buf
			bufPool.Put(bp)
		}()
	}

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if hasBinaryNull(probe) {
		return Result{Path: path}
	}

	return Result{Path: path, Count: matcher.CountMatchingWordLines(buf, s.m)}
}

// SearchFileCountByLine counts matching lines with line-scoped regex semantics.
func (s *Searcher) SearchFileCountByLine(path string, fileSize int64, invert bool) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	if !fromMmap {
		defer func() {
			bp := &buf
			bufPool.Put(bp)
		}()
	}

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if hasBinaryNull(probe) {
		return Result{Path: path}
	}

	if invert {
		return Result{Path: path, Count: matcher.CountNonMatchingLinesByLine(buf, s.m)}
	}
	return Result{Path: path, Count: matcher.CountMatchingLinesByLine(buf, s.m)}
}

// SearchFileExists tests whether a file has any match without building output.
func (s *Searcher) SearchFileExists(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	if !fromMmap {
		defer func() {
			bp := &buf
			bufPool.Put(bp)
		}()
	}

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if hasBinaryNull(probe) {
		return Result{Path: path}
	}

	start, _ := s.m.Match(buf)
	if start < 0 {
		return Result{Path: path}
	}
	return Result{Path: path, Count: 1}
}

func readSearchBuffer(path string, fileSize int64) ([]byte, func(), bool, error) {
	if fileSize >= mmapThreshold && fileSize <= maxMmapSize && runtime.GOOS != "windows" {
		b, fn, err := mmapFile(path)
		if err == nil {
			return b, fn, true, nil
		}
	}

	buf, err := readFilePooled(path, fileSize)
	if err != nil {
		return nil, nil, false, err
	}
	return buf, nil, false, nil
}

func mmapFile(path string) ([]byte, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	sz := fi.Size()
	if sz <= 0 {
		f.Close()
		return nil, nil, os.ErrInvalid
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(sz), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return b, func() {
		_ = syscall.Munmap(b)
		f.Close()
	}, nil
}

func readFilePooled(path string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	bufp := bufPool.Get().(*[]byte)
	buf := *bufp
	need := int(size)
	if need > len(buf) {
		buf = make([]byte, need)
		*bufp = buf
	}
	buf = buf[:need]
	n, err := io.ReadFull(f, buf)
	if err != nil && n == 0 {
		return nil, err
	}
	return buf[:n], nil
}

func hasBinaryNull(b []byte) bool {
	i := 0
	for ; i+8 <= len(b); i += 8 {
		v := uint64(b[i]) | uint64(b[i+1])<<8 | uint64(b[i+2])<<16 |
			uint64(b[i+3])<<24 | uint64(b[i+4])<<32 | uint64(b[i+5])<<40 |
			uint64(b[i+6])<<48 | uint64(b[i+7])<<56
		const mask = 0x7f7f7f7f7f7f7f7f
		if (v-0x0101010101010101)&^(v|mask) != 0 {
			return true
		}
	}
	for ; i < len(b); i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}
