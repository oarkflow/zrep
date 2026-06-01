// Package searcher provides the core file-search engine.
package searcher

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"

	"github.com/zrep/zrep/internal/matcher"
)

var errTooLargeForMultiline = errors.New("file too large for buffered multiline search")

// Result holds all matches for a single file.
type Result struct {
	Path      string
	Matches   []matcher.LineMatch
	OnlyMatch []byte
	Count     int
	Err       error
	Cleanup   func()
	Force     bool
}

// Options controls generic line-oriented search features.
type Options struct {
	Invert   bool
	Passthru bool
	MaxCount int
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
	Encoding     string
}

// New creates a Searcher.
func New(m matcher.Matcher) *Searcher {
	return &Searcher{m: m}
}

// IsSupportedArchive reports whether path can be searched by SearchArchive.
func IsSupportedArchive(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".gz" || ext == ".zip"
}

// SearchFile searches a single file and returns its Result.
func (s *Searcher) SearchFile(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStream(path, false)
	}

	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
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

	if cleanup != nil {
		copyLineMatches(lm)
		cleanup()
		cleanup = nil
	}

	return Result{Path: path, Matches: lm, Cleanup: cleanup}
}

// SearchReader searches a stream. It is used for stdin and therefore avoids
// mmap/pooling assumptions.
func (s *Searcher) SearchReader(path string, r io.Reader, opts Options) Result {
	lm, count, err := s.searchReader(path, r, opts, nil)
	return Result{Path: path, Matches: lm, Count: count, Err: err}
}

// SearchFileOptions searches a file with generic line-oriented options.
func (s *Searcher) SearchFileOptions(path string, fileSize int64, opts Options) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		var out []matcher.LineMatch
		r := s.searchFileStreamOptions(path, opts, func(chunk Result) {
			out = append(out, chunk.Matches...)
		})
		if r.Err != nil {
			return r
		}
		return Result{Path: path, Matches: out, Count: len(out)}
	}
	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if !s.SearchBinary && hasBinaryNull(firstBytes(buf, 8192)) {
		return Result{Path: path}
	}
	return s.searchBufferOptions(path, buf, opts)
}

// SearchFileCountMatches counts individual matches, not matching lines.
func (s *Searcher) SearchFileCountMatches(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		f, err := os.Open(path)
		if err != nil {
			return Result{Path: path, Err: err}
		}
		defer f.Close()
		count, err := s.countReaderMatches(bufio.NewReaderSize(f, 1<<20))
		return Result{Path: path, Count: count, Err: err}
	}
	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if !s.SearchBinary && hasBinaryNull(firstBytes(buf, 8192)) {
		return Result{Path: path}
	}
	count := 0
	for _, line := range splitLines(buf) {
		count += len(s.m.MatchAll(line, nil))
	}
	return Result{Path: path, Count: count}
}

// SearchFileCountLiteralMatches counts exact non-overlapping literal matches.
func (s *Searcher) SearchFileCountLiteralMatches(path string, fileSize int64, literal string) Result {
	needle := []byte(literal)
	if len(needle) == 0 || fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
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
				count += bytes.Count(line, needle)
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
	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()
	if !s.SearchBinary && hasBinaryNull(firstBytes(buf, 8192)) {
		return Result{Path: path}
	}
	return Result{Path: path, Count: bytes.Count(buf, needle)}
}

func (s *Searcher) countReaderMatches(r *bufio.Reader) (int, error) {
	if !s.SearchBinary && streamHasBinaryNull(r) {
		return 0, nil
	}
	count := 0
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			count += len(s.m.MatchAll(trimLineBreak(line), nil))
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return count, readErr
		}
	}
	return count, nil
}

// SearchFileMultiline searches the whole file as one buffer and reports
// multiline matches at their starting line and column.
func (s *Searcher) SearchFileMultiline(path string, fileSize int64) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return Result{Path: path, Err: errTooLargeForMultiline}
	}
	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer cleanup()
	if !s.SearchBinary && hasBinaryNull(firstBytes(buf, 8192)) {
		return Result{Path: path}
	}
	matches := s.m.MatchAll(buf, nil)
	out := make([]matcher.LineMatch, 0, len(matches))
	for _, m := range matches {
		lineNum, lineStart := lineAtOffset(buf, m.Start)
		lineEnd := m.End
		if lineEnd > len(buf) {
			lineEnd = len(buf)
		}
		out = append(out, matcher.LineMatch{
			LineNum: lineNum,
			Line:    append([]byte(nil), buf[lineStart:lineEnd]...),
			Matches: []matcher.Match{{Start: m.Start - lineStart, End: lineEnd - lineStart}},
		})
	}
	return Result{Path: path, Matches: out}
}

// SearchArchive searches .gz and .zip contents using portable stdlib readers.
func (s *Searcher) SearchArchive(path string) []Result {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".gz":
		return []Result{s.searchGzip(path)}
	case ".zip":
		return s.searchZip(path)
	default:
		return nil
	}
}

// SearchFileContext returns matching lines plus surrounding context.
func (s *Searcher) SearchFileContext(path string, fileSize int64, before, after int) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStream(path, false)
	}
	if before < 0 {
		before = 0
	}
	if after < 0 {
		after = 0
	}

	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
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

	lines := splitLines(buf)
	out := make([]matcher.LineMatch, 0, 8)
	lastAdded := 0
	matchBuf := make([]matcher.Match, 0, 8)
	for i, line := range lines {
		matchBuf = matchBuf[:0]
		matchBuf = s.m.MatchAll(line, matchBuf)
		if len(matchBuf) == 0 {
			continue
		}

		start := i - before
		if start < 0 {
			start = 0
		}
		for j := start; j < i && j+1 > lastAdded; j++ {
			out = append(out, matcher.LineMatch{LineNum: j + 1, Line: lines[j], Context: true})
			lastAdded = j + 1
		}

		ms := make([]matcher.Match, len(matchBuf))
		copy(ms, matchBuf)
		out = append(out, matcher.LineMatch{LineNum: i + 1, Line: line, Matches: ms})
		lastAdded = i + 1

		end := i + after
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for j := i + 1; j <= end && j+1 > lastAdded; j++ {
			out = append(out, matcher.LineMatch{LineNum: j + 1, Line: lines[j], Context: true})
			lastAdded = j + 1
		}
	}

	if cleanup != nil {
		copyLineMatches(out)
		cleanup()
	}
	return Result{Path: path, Matches: out}
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

	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
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

// ReplaceFile applies replacement to all non-overlapping matches in a regular file.
func (s *Searcher) ReplaceFile(path string, fileSize int64, replacement string) Result {
	if fileSize == 0 || fileSize > maxMmapSize {
		return Result{Path: path}
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if decoded, ok := s.decodeText(buf); ok {
		buf = decoded
	}
	if !s.SearchBinary && hasBinaryNull(firstBytes(buf, 8192)) {
		return Result{Path: path}
	}
	matches := s.m.MatchAll(buf, nil)
	if len(matches) == 0 {
		return Result{Path: path}
	}
	repl := []byte(replacement)
	out := make([]byte, 0, len(buf)+len(matches)*len(repl))
	pos := 0
	for _, m := range matches {
		if m.Start < pos {
			continue
		}
		out = append(out, buf[pos:m.Start]...)
		out = append(out, repl...)
		pos = m.End
	}
	out = append(out, buf[pos:]...)
	mode := os.FileMode(0o666)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(path, out, mode); err != nil {
		return Result{Path: path, Err: err}
	}
	return Result{Path: path, Count: len(matches)}
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

	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
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
	if cleanup != nil {
		copyLineMatches(lm)
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

	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
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

	if cleanup != nil {
		copyLineMatches(lm)
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

	buf, cleanup, fromMmap, err := s.readSearchBuffer(path, fileSize)
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

	buf, cleanup, fromMmap, err := s.readSearchBuffer(path, fileSize)
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

// SearchFileCountExactLine counts lines exactly equal to literal.
func (s *Searcher) SearchFileCountExactLine(path string, fileSize int64, literal string) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	needle := []byte(literal)
	if fileSize > maxMmapSize {
		return s.searchFileStreamCountExactLine(path, needle)
	}

	buf, cleanup, _, err := s.readSearchBuffer(path, fileSize)
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

	return Result{Path: path, Count: countExactLine(buf, needle)}
}

// SearchFileCountByLine counts matching lines with line-scoped regex semantics.
func (s *Searcher) SearchFileCountByLine(path string, fileSize int64, invert bool) Result {
	if fileSize == 0 {
		return Result{Path: path}
	}
	if fileSize > maxMmapSize {
		return s.searchFileStreamCount(path, invert)
	}

	buf, cleanup, fromMmap, err := s.readSearchBuffer(path, fileSize)
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
	if prefix, ok := matcher.LiteralPrefix(s.m); ok {
		return Result{Path: path, Count: countMatchingLinesByPrefix(buf, s.m, []byte(prefix))}
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

	buf, cleanup, fromMmap, err := s.readSearchBuffer(path, fileSize)
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

func (s *Searcher) searchFileStreamOptions(path string, opts Options, emit func(Result)) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()

	if !s.SearchBinary {
		r := bufio.NewReaderSize(f, 1<<20)
		if streamHasBinaryNull(r) {
			return Result{Path: path}
		}
		_, _ = f.Seek(0, io.SeekStart)
	}

	var batch []matcher.LineMatch
	total := 0
	_, _, err = s.searchReader(path, bufio.NewReaderSize(f, 1<<20), opts, func(lm matcher.LineMatch) bool {
		batch = append(batch, lm)
		if !lm.Context {
			total++
		}
		if len(batch) >= 256 {
			emit(Result{Path: path, Matches: batch, Count: total})
			batch = make([]matcher.LineMatch, 0, 256)
		}
		return true
	})
	if err != nil {
		return Result{Path: path, Err: err}
	}
	if len(batch) > 0 {
		emit(Result{Path: path, Matches: batch, Count: total})
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

func (s *Searcher) searchFileStreamCountExactLine(path string, needle []byte) Result {
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
			if bytes.Equal(line, needle) {
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

	for lineStart < len(buf) {
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

func copyLineMatches(matches []matcher.LineMatch) {
	for i := range matches {
		cp := make([]byte, len(matches[i].Line))
		copy(cp, matches[i].Line)
		matches[i].Line = cp
	}
}

func splitLines(buf []byte) [][]byte {
	lines := make([][]byte, 0, bytes.Count(buf, []byte{'\n'})+1)
	lineStart := 0
	for lineStart < len(buf) {
		lineEndRel := bytes.IndexByte(buf[lineStart:], '\n')
		lineEnd := len(buf)
		if lineEndRel >= 0 {
			lineEnd = lineStart + lineEndRel
		}
		line := buf[lineStart:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, line)
		if lineEndRel < 0 {
			break
		}
		lineStart = lineEnd + 1
	}
	return lines
}

func countMatchingLinesByPrefix(buf []byte, m matcher.Matcher, prefix []byte) int {
	if len(prefix) == 0 {
		return matcher.CountMatchingLinesByLine(buf, m)
	}

	count := 0
	offset := 0
	for offset < len(buf) {
		idx := bytes.Index(buf[offset:], prefix)
		if idx < 0 {
			break
		}
		candidate := offset + idx
		lineStart := candidate
		for lineStart > 0 && buf[lineStart-1] != '\n' {
			lineStart--
		}
		lineEnd := candidate + len(prefix)
		for lineEnd < len(buf) && buf[lineEnd] != '\n' {
			lineEnd++
		}

		if start, _ := m.Match(buf[lineStart:lineEnd]); start >= 0 {
			count++
			offset = lineEnd + 1
			continue
		}
		offset = candidate + 1
	}
	return count
}

func countExactLine(buf, needle []byte) int {
	count := 0
	lineStart := 0
	for lineStart <= len(buf) {
		lineEndRel := bytes.IndexByte(buf[lineStart:], '\n')
		lineEnd := len(buf)
		if lineEndRel >= 0 {
			lineEnd = lineStart + lineEndRel
		}
		line := buf[lineStart:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if bytes.Equal(line, needle) {
			count++
		}
		if lineEndRel < 0 {
			break
		}
		lineStart = lineEnd + 1
	}
	return count
}

func firstBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func lineAtOffset(buf []byte, offset int) (int, int) {
	lineNum := 1
	lineStart := 0
	for i := 0; i < offset && i < len(buf); i++ {
		if buf[i] == '\n' {
			lineNum++
			lineStart = i + 1
		}
	}
	return lineNum, lineStart
}

func (s *Searcher) decodeText(buf []byte) ([]byte, bool) {
	encoding := strings.ToLower(s.Encoding)
	if encoding == "" || encoding == "auto" {
		if len(buf) >= 2 {
			if buf[0] == 0xff && buf[1] == 0xfe {
				return decodeUTF16(buf[2:], true), true
			}
			if buf[0] == 0xfe && buf[1] == 0xff {
				return decodeUTF16(buf[2:], false), true
			}
		}
		return nil, false
	}
	switch encoding {
	case "utf-8", "utf8":
		return nil, false
	case "utf-16le", "utf16le":
		return decodeUTF16(trimUTF16BOM(buf), true), true
	case "utf-16be", "utf16be":
		return decodeUTF16(trimUTF16BOM(buf), false), true
	default:
		return nil, false
	}
}

func trimUTF16BOM(buf []byte) []byte {
	if len(buf) >= 2 && (buf[0] == 0xff && buf[1] == 0xfe || buf[0] == 0xfe && buf[1] == 0xff) {
		return buf[2:]
	}
	return buf
}

func decodeUTF16(buf []byte, littleEndian bool) []byte {
	u16 := make([]uint16, 0, len(buf)/2)
	for i := 0; i+1 < len(buf); i += 2 {
		if littleEndian {
			u16 = append(u16, uint16(buf[i])|uint16(buf[i+1])<<8)
		} else {
			u16 = append(u16, uint16(buf[i])<<8|uint16(buf[i+1]))
		}
	}
	return []byte(string(utf16.Decode(u16)))
}

func (s *Searcher) searchGzip(path string) Result {
	f, err := os.Open(path)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	defer gr.Close()
	buf, err := io.ReadAll(gr)
	if err != nil {
		return Result{Path: path, Err: err}
	}
	return s.searchBuffer(path, buf)
}

func (s *Searcher) searchZip(path string) []Result {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return []Result{{Path: path, Err: err}}
	}
	defer zr.Close()
	results := make([]Result, 0)
	for _, file := range zr.File {
		if file.FileInfo().IsDir() {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			results = append(results, Result{Path: path + "::" + filepath.ToSlash(file.Name), Err: err})
			continue
		}
		buf, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			results = append(results, Result{Path: path + "::" + filepath.ToSlash(file.Name), Err: err})
			continue
		}
		r := s.searchBuffer(path+"::"+filepath.ToSlash(file.Name), buf)
		if r.Count > 0 || len(r.Matches) > 0 || r.Err != nil {
			results = append(results, r)
		}
	}
	return results
}

func (s *Searcher) searchBuffer(path string, buf []byte) Result {
	if decoded, ok := s.decodeText(buf); ok {
		buf = decoded
	}
	if !s.SearchBinary && hasBinaryNull(firstBytes(buf, 8192)) {
		return Result{Path: path}
	}
	lm := make([]matcher.LineMatch, 0, 8)
	mb := make([]matcher.Match, 0, 8)
	lm, _ = matcher.FindMatchingLines(buf, s.m, 0, lm, mb)
	copyLineMatches(lm)
	return Result{Path: path, Matches: lm, Count: len(lm)}
}

func (s *Searcher) searchBufferOptions(path string, buf []byte, opts Options) Result {
	out := make([]matcher.LineMatch, 0, 8)
	matchCount := 0
	lineNum := 0
	lineStart := 0
	for lineStart < len(buf) {
		lineEndRel := bytes.IndexByte(buf[lineStart:], '\n')
		lineEnd := len(buf)
		if lineEndRel >= 0 {
			lineEnd = lineStart + lineEndRel
		}
		line := buf[lineStart:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lineNum++
		matches := s.m.MatchAll(line, nil)
		isMatch := len(matches) > 0
		include := isMatch
		if opts.Invert {
			include = !isMatch
		}
		if opts.Passthru {
			include = true
		}
		if !include {
			if lineEndRel < 0 {
				break
			}
			lineStart = lineEnd + 1
			continue
		}
		lm := matcher.LineMatch{LineNum: lineNum, Line: line}
		if isMatch && !opts.Invert {
			lm.Matches = append([]matcher.Match(nil), matches...)
			matchCount++
		} else if opts.Passthru {
			lm.Context = true
		}
		out = append(out, lm)
		if opts.MaxCount > 0 && matchCount >= opts.MaxCount {
			break
		}
		if lineEndRel < 0 {
			break
		}
		lineStart = lineEnd + 1
	}
	copyLineMatches(out)
	return Result{Path: path, Matches: out, Count: matchCount}
}

func (s *Searcher) searchReader(path string, r io.Reader, opts Options, emit func(matcher.LineMatch) bool) ([]matcher.LineMatch, int, error) {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20)
	}
	out := make([]matcher.LineMatch, 0, 64)
	matchBuf := make([]matcher.Match, 0, 8)
	lineNum := 0
	matchCount := 0
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			lineNum++
			line = trimLineBreak(line)
			matchBuf = matchBuf[:0]
			matchBuf = s.m.MatchAll(line, matchBuf)
			isMatch := len(matchBuf) > 0
			include := isMatch
			if opts.Invert {
				include = !isMatch
			}
			if opts.Passthru {
				include = true
			}
			if include {
				lm := matcher.LineMatch{LineNum: lineNum, Line: append([]byte(nil), line...)}
				if isMatch && !opts.Invert {
					lm.Matches = append([]matcher.Match(nil), matchBuf...)
					matchCount++
				} else if opts.Passthru {
					lm.Context = true
				}
				if emit != nil {
					if !emit(lm) {
						return nil, matchCount, nil
					}
				} else {
					out = append(out, lm)
				}
				if opts.MaxCount > 0 && matchCount >= opts.MaxCount {
					return out, matchCount, nil
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return out, matchCount, readErr
		}
	}
	return out, matchCount, nil
}

func (s *Searcher) readSearchBuffer(path string, fileSize int64) ([]byte, func(), bool, error) {
	buf, cleanup, fromMmap, err := readSearchBuffer(path, fileSize)
	if err != nil {
		return nil, nil, false, err
	}
	decoded, ok := s.decodeText(buf)
	if !ok {
		return buf, cleanup, fromMmap, nil
	}
	if cleanup != nil {
		cleanup()
	}
	return decoded, nil, false, nil
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
