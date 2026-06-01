// Package searcher provides the core file-search engine.
package searcher

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/zrep/zrep/internal/matcher"
)

// Result holds all matches for a single file.
type Result struct {
	Path      string
	Matches   []matcher.LineMatch
	OnlyMatch []byte
	Count     int
	Err       error
	Cleanup   func()
}

// mmap files >= 32KB; use pooled reads for smaller ones to avoid syscall overhead.
const mmapThreshold = 32 * 1024
const maxMmapSize = 512 << 20

// MaxBufferedFileSize is the largest file zrep reads as one buffer.
// Larger files are searched line-by-line so multi-GB and TB-scale files do not
// require proportional memory.
const MaxBufferedFileSize = maxMmapSize

var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 1<<20)
		return &b
	},
}

// Searcher searches files using a Matcher.
type Searcher struct {
	m            matcher.Matcher
	SearchBinary bool
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
	if fileSize > maxMmapSize {
		return s.searchFileStream(path, false)
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}

	// Skip binary files: scan first 8KB for null bytes
	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
		if cleanup != nil {
			cleanup()
		}
		return Result{Path: path}
	}

	lm := make([]matcher.LineMatch, 0, 8)
	mb := make([]matcher.Match, 0, 8)
	lm, _ = matcher.FindMatchingLines(buf, s.m, 0, lm, mb)

	if fromMmap {
		for i := range lm {
			cp := make([]byte, len(lm[i].Line))
			copy(cp, lm[i].Line)
			lm[i].Line = cp
		}
		cleanup()
		cleanup = nil
	}

	return Result{Path: path, Matches: lm, Cleanup: cleanup}
}

// SearchFileOnlyLiteral counts exact non-overlapping literal matches for the
// fast single-pattern -F -o path without extracting line metadata.
func (s *Searcher) SearchFileOnlyLiteral(path string, fileSize int64, literal string) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStreamOnlyLiteral(path, literal)
	}

	buf, cleanup, _, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer cleanup()

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
		return Result{Path: path}
	}

	return Result{Path: path, Count: bytes.Count(buf, []byte(literal)), OnlyMatch: []byte(literal)}
}

// SearchFileLines searches a file and returns matching lines without match
// ranges. It is faster for no-color whole-line output.
func (s *Searcher) SearchFileLines(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStreamLines(path, false)
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
		if cleanup != nil {
			cleanup()
		}
		return Result{Path: path}
	}

	lm := findMatchingLineOnly(buf, s.m, 0, make([]matcher.LineMatch, 0, 8))
	if fromMmap {
		for i := range lm {
			cp := make([]byte, len(lm[i].Line))
			copy(cp, lm[i].Line)
			lm[i].Line = cp
		}
		cleanup()
		cleanup = nil
	}
	return Result{Path: path, Matches: lm, Cleanup: cleanup}
}

// SearchFileChunks streams matching lines for large files and calls emit for
// each bounded batch. It returns a final Result only for errors.
func (s *Searcher) SearchFileChunks(path string, invert bool, emit func(Result)) Result {
	return s.searchFileStreamChunks(path, invert, emit)
}

// SearchFileLineChunks streams matching lines without match ranges.
func (s *Searcher) SearchFileLineChunks(path string, invert bool, emit func(Result)) Result {
	return s.searchFileStreamLineChunks(path, invert, emit)
}

// SearchFileInvert returns lines that do not match.
func (s *Searcher) SearchFileInvert(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStream(path, true)
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
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
		cleanup = nil
	}

	return Result{Path: path, Matches: lm, Cleanup: cleanup}
}

// SearchFileCount counts matching lines without materializing line matches.
func (s *Searcher) SearchFileCount(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStreamCount(path, false)
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	_ = fromMmap

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
		return Result{Path: path}
	}

	return Result{Path: path, Count: matcher.CountMatchingLines(buf, s.m)}
}

// SearchFileCountWords counts matching lines with ASCII whole-word boundaries.
func (s *Searcher) SearchFileCountWords(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStreamCountWords(path)
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	_ = fromMmap

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
		return Result{Path: path}
	}

	return Result{Path: path, Count: matcher.CountMatchingWordLines(buf, s.m)}
}

// SearchFileCountByLine counts matching lines with line-scoped regex semantics.
func (s *Searcher) SearchFileCountByLine(path string, fileSize int64, invert bool) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStreamCount(path, invert)
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	_ = fromMmap

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
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
	if fileSize > maxMmapSize {
		return s.searchFileStreamExists(path)
	}

	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if cleanup != nil {
		defer cleanup()
	}
	_ = fromMmap

	probe := buf
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if !s.SearchBinary && hasBinaryNull(probe) {
		return Result{Path: path}
	}

	start, _ := s.m.Match(buf)
	if start < 0 {
		return Result{Path: path}
	}
	return Result{Path: path, Count: 1}
}

func (s *Searcher) searchFileStream(path string, invert bool) Result {
	var all []matcher.LineMatch
	result := s.searchFileStreamChunks(path, invert, func(r Result) {
		all = append(all, r.Matches...)
	})
	if result.Err != nil {
		return result
	}
	return Result{Path: path, Matches: all}
}

func (s *Searcher) searchFileStreamLines(path string, invert bool) Result {
	var all []matcher.LineMatch
	result := s.searchFileStreamLineChunks(path, invert, func(r Result) {
		all = append(all, r.Matches...)
	})
	if result.Err != nil {
		return result
	}
	return Result{Path: path, Matches: all}
}

func (s *Searcher) searchFileStreamChunks(path string, invert bool, emit func(Result)) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	if !s.SearchBinary && streamHasBinaryNull(r) {
		return Result{Path: path}
	}

	lm := make([]matcher.LineMatch, 0, 256)
	matchBuf := make([]matcher.Match, 0, 8)
	lineNum := 0

	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			lineNum++
			line = trimLineBreak(line)
			matchBuf = matchBuf[:0]
			matchBuf = s.m.MatchAll(line, matchBuf)
			if invert {
				if len(matchBuf) == 0 {
					lm = append(lm, matcher.LineMatch{LineNum: lineNum, Line: line})
				}
			} else if len(matchBuf) > 0 {
				ms := make([]matcher.Match, len(matchBuf))
				copy(ms, matchBuf)
				lm = append(lm, matcher.LineMatch{LineNum: lineNum, Line: line, Matches: ms})
			}
			if len(lm) >= 256 {
				emit(Result{Path: path, Matches: lm, Count: len(lm)})
				lm = make([]matcher.LineMatch, 0, 256)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Result{Path: path, Err: readErr}
		}
	}
	if len(lm) > 0 {
		emit(Result{Path: path, Matches: lm, Count: len(lm)})
	}
	return Result{Path: path}
}

func (s *Searcher) searchFileStreamLineChunks(path string, invert bool, emit func(Result)) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	if !s.SearchBinary && streamHasBinaryNull(r) {
		return Result{Path: path}
	}

	lm := make([]matcher.LineMatch, 0, 256)
	lineNum := 0

	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			lineNum++
			line = trimLineBreak(line)
			start, _ := s.m.Match(line)
			if invert && start < 0 || !invert && start >= 0 {
				lm = append(lm, matcher.LineMatch{LineNum: lineNum, Line: line})
			}
			if len(lm) >= 256 {
				emit(Result{Path: path, Matches: lm, Count: len(lm)})
				lm = make([]matcher.LineMatch, 0, 256)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Result{Path: path, Err: readErr}
		}
	}
	if len(lm) > 0 {
		emit(Result{Path: path, Matches: lm, Count: len(lm)})
	}
	return Result{Path: path}
}

func (s *Searcher) searchFileStreamCount(path string, invert bool) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	if !s.SearchBinary && streamHasBinaryNull(r) {
		return Result{Path: path}
	}

	count := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineBreak(line)
			start, _ := s.m.Match(line)
			if invert && start < 0 || !invert && start >= 0 {
				count++
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Result{Path: path, Err: readErr}
		}
	}
	return Result{Path: path, Count: count}
}

func (s *Searcher) searchFileStreamCountWords(path string) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	if !s.SearchBinary && streamHasBinaryNull(r) {
		return Result{Path: path}
	}

	count := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineBreak(line)
			count += matcher.CountMatchingWordLines(line, s.m)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Result{Path: path, Err: readErr}
		}
	}
	return Result{Path: path, Count: count}
}

func (s *Searcher) searchFileStreamOnlyLiteral(path, literal string) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	if !s.SearchBinary && streamHasBinaryNull(r) {
		return Result{Path: path}
	}

	needle := []byte(literal)
	count := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			count += bytes.Count(line, needle)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Result{Path: path, Err: readErr}
		}
	}
	return Result{Path: path, Count: count, OnlyMatch: needle}
}

func (s *Searcher) searchFileStreamExists(path string) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<20)
	if !s.SearchBinary && streamHasBinaryNull(r) {
		return Result{Path: path}
	}

	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			line = trimLineBreak(line)
			start, _ := s.m.Match(line)
			if start >= 0 {
				return Result{Path: path, Count: 1}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return Result{Path: path, Err: readErr}
		}
	}
	return Result{Path: path}
}

func trimLineBreak(line []byte) []byte {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return line
}

func streamHasBinaryNull(r *bufio.Reader) bool {
	probe, err := r.Peek(8192)
	if err != nil && len(probe) == 0 {
		return false
	}
	return hasBinaryNull(probe)
}

func findMatchingLineOnly(buf []byte, m matcher.Matcher, lineOffset int, dst []matcher.LineMatch) []matcher.LineMatch {
	lineStart := 0
	lineNum := lineOffset

	for lineStart <= len(buf) {
		lineEnd := lineStart
		for lineEnd < len(buf) && buf[lineEnd] != '\n' {
			lineEnd++
		}

		line := buf[lineStart:lineEnd]
		start, _ := m.Match(line)
		if start >= 0 {
			dst = append(dst, matcher.LineMatch{
				LineNum: lineNum + 1,
				Line:    line,
			})
		}

		lineNum++
		if lineEnd >= len(buf) {
			break
		}
		lineStart = lineEnd + 1
	}
	return dst
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
	return buf, func() {
		bp := &buf
		bufPool.Put(bp)
	}, false, nil
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
