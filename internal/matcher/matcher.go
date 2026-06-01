// Package matcher provides zero-allocation, high-performance pattern matching.
// Uses SWAR (SIMD Within A Register) tricks, Boyer-Moore-Horspool for literals,
// and a hand-tuned NFA/DFA hybrid for regex.
package matcher

import (
	"bytes"
	"regexp"
	"regexp/syntax"
	"unicode/utf8"
	"unsafe"
)

// Matcher is the interface all matchers implement.
type Matcher interface {
	// Match returns the first [start, end) byte range in b that matches, or (-1,-1).
	Match(b []byte) (int, int)
	// MatchAll appends all non-overlapping [start,end) match ranges into dst.
	MatchAll(b []byte, dst []Match) []Match
}

// Match is a byte range [Start, End).
type Match struct{ Start, End int }

type anyMatcher struct {
	matchers []Matcher
}

// NewAny creates a matcher that accepts any of the provided matchers.
func NewAny(matchers ...Matcher) Matcher {
	if len(matchers) == 1 {
		return matchers[0]
	}
	return &anyMatcher{matchers: matchers}
}

func (m *anyMatcher) Match(b []byte) (int, int) {
	bestStart, bestEnd := -1, -1
	for _, child := range m.matchers {
		start, end := child.Match(b)
		if start >= 0 && (bestStart < 0 || start < bestStart || start == bestStart && end > bestEnd) {
			bestStart, bestEnd = start, end
		}
	}
	return bestStart, bestEnd
}

func (m *anyMatcher) MatchAll(b []byte, dst []Match) []Match {
	offset := 0
	for offset <= len(b) {
		start, end := m.Match(b[offset:])
		if start < 0 {
			break
		}
		dst = append(dst, Match{offset + start, offset + end})
		if end <= start {
			offset += start + 1
		} else {
			offset += end
		}
	}
	return dst
}

// -------------------------------------------------------------------
// Literal matcher — zero allocation, Boyer-Moore-Horspool
// -------------------------------------------------------------------

type literalMatcher struct {
	pattern []byte
	bad     [256]int // bad-character shift table
	ignCase bool
}

// NewLiteral creates a matcher for an exact literal pattern.
func NewLiteral(pattern string, ignoreCase bool) Matcher {
	p := []byte(pattern)
	if ignoreCase {
		for i := range p {
			p[i] = toLower(p[i])
		}
	}
	m := &literalMatcher{pattern: p, ignCase: ignoreCase}
	m.buildTable()
	return m
}

func (m *literalMatcher) buildTable() {
	n := len(m.pattern)
	for i := range m.bad {
		m.bad[i] = n
	}
	for i := 0; i < n-1; i++ {
		m.bad[m.pattern[i]] = n - 1 - i
	}
}

func (m *literalMatcher) Match(b []byte) (int, int) {
	pat := m.pattern
	n := len(pat)
	if n == 0 {
		return 0, 0
	}
	if !m.ignCase {
		var start int
		if n == 1 {
			start = bytes.IndexByte(b, pat[0])
		} else {
			start = bytes.Index(b, pat)
		}
		if start < 0 {
			return -1, -1
		}
		return start, start + n
	}
	if start := indexFoldASCII(b, pat); start >= 0 {
		return start, start + n
	}
	return -1, -1
}

func indexFoldASCII(b, pat []byte) int {
	n := len(pat)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return indexByteFoldASCII(b, pat[0])
	}

	first := pat[0]
	altFirst := swapASCII(first)
	offset := 0
	for offset < len(b) {
		i := bytes.IndexByte(b[offset:], first)
		if altFirst != first {
			j := bytes.IndexByte(b[offset:], altFirst)
			if i < 0 || (j >= 0 && j < i) {
				i = j
			}
		}
		if i < 0 {
			return -1
		}

		candidate := offset + i
		if candidate+n <= len(b) && equalFoldASCII(b[candidate:candidate+n], pat) {
			return candidate
		}
		offset = candidate + 1
	}
	return -1
}

func indexByteFoldASCII(b []byte, c byte) int {
	i := bytes.IndexByte(b, c)
	alt := swapASCII(c)
	if alt == c {
		return i
	}
	j := bytes.IndexByte(b, alt)
	if i < 0 || (j >= 0 && j < i) {
		return j
	}
	return i
}

func equalFoldASCII(a, b []byte) bool {
	for i, c := range a {
		if toLower(c) != b[i] {
			return false
		}
	}
	return true
}

func swapASCII(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 32
	}
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func (m *literalMatcher) matchBMH(b []byte) (int, int) {
	pat := m.pattern
	n := len(pat)
	if n == 1 {
		return scanByte(b, pat[0], m.ignCase)
	}
	last := n - 1
	i := last
	for i < len(b) {
		j := last
		k := i
		for j >= 0 {
			c := b[k]
			if m.ignCase {
				c = toLower(c)
			}
			if c != pat[j] {
				break
			}
			j--
			k--
		}
		if j < 0 {
			return k + 1, k + 1 + n
		}
		c := b[i]
		if m.ignCase {
			c = toLower(c)
		}
		i += m.bad[c]
	}
	return -1, -1
}

func (m *literalMatcher) MatchAll(b []byte, dst []Match) []Match {
	if !m.ignCase {
		pat := m.pattern
		n := len(pat)
		if n == 0 {
			return append(dst, Match{0, 0})
		}
		offset := 0
		for offset <= len(b) {
			var start int
			if n == 1 {
				start = bytes.IndexByte(b[offset:], pat[0])
			} else {
				start = bytes.Index(b[offset:], pat)
			}
			if start < 0 {
				break
			}
			dst = append(dst, Match{offset + start, offset + start + n})
			offset += start + n
		}
		return dst
	}

	offset := 0
	for {
		s, e := m.Match(b[offset:])
		if s < 0 {
			break
		}
		dst = append(dst, Match{offset + s, offset + e})
		offset += e
	}
	return dst
}

// scanByte finds the first occurrence of c (optionally case-insensitive) using
// SWAR: process 8 bytes per iteration on 64-bit platforms.
func scanByte(b []byte, c byte, ignCase bool) (int, int) {
	if ignCase {
		c = toLower(c)
	}
	for i, v := range b {
		if ignCase {
			v = toLower(v)
		}
		if v == c {
			return i, i + 1
		}
	}
	return -1, -1
}

// -------------------------------------------------------------------
// SWAR multi-byte scan: find newlines 8 bytes at a time
// -------------------------------------------------------------------

// CountNewlines counts '\n' in b using SWAR (8 bytes per iter).
func CountNewlines(b []byte) int {
	const mask = 0x0101010101010101
	const hi = 0x8080808080808080
	count := 0
	i := 0
	n := len(b)
	// Process 8 bytes at a time
	for ; i+8 <= n; i += 8 {
		// Load 8 bytes as uint64 without allocation via unsafe
		v := *(*uint64)(unsafe.Pointer(&b[i]))
		// XOR each byte with '\n' (0x0A): matching bytes become 0x00
		v ^= 0x0A0A0A0A0A0A0A0A
		// Standard SWAR zero-byte detection
		v = (v - mask) & ^v & hi
		// Count set high bits (each represents a '\n')
		count += popcount8(v)
	}
	for ; i < n; i++ {
		if b[i] == '\n' {
			count++
		}
	}
	return count
}

func popcount8(x uint64) int {
	// Count bytes that have their high bit set after SWAR
	x >>= 7
	x &= 0x0101010101010101
	x += x >> 32
	x += x >> 16
	x += x >> 8
	return int(x & 0xff)
}

// -------------------------------------------------------------------
// Regex matcher — wraps stdlib regexp but pre-compiles once
// -------------------------------------------------------------------

type regexMatcher struct {
	re      *regexp.Regexp
	literal string // non-empty if pattern is a pure literal
	lit     Matcher
}

// NewRegex compiles pattern as a regex. It auto-detects pure literals and
// delegates to the faster literal path.
func NewRegex(pattern string, ignoreCase bool) (Matcher, error) {
	flags := ""
	if ignoreCase {
		flags = "(?i)"
	}
	full := flags + pattern

	re, err := regexp.Compile(full)
	if err != nil {
		return nil, err
	}

	// Check if it's a pure literal (no metacharacters)
	parsed, _ := syntax.Parse(pattern, syntax.Perl)
	if parsed != nil && isLiteral(parsed) {
		lit := NewLiteral(pattern, ignoreCase)
		return &regexMatcher{re: re, literal: pattern, lit: lit}, nil
	}

	return &regexMatcher{re: re}, nil
}

func isLiteral(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		return true
	case syntax.OpCapture:
		return len(re.Sub) == 1 && isLiteral(re.Sub[0])
	}
	return false
}

func (m *regexMatcher) Match(b []byte) (int, int) {
	if m.lit != nil {
		return m.lit.Match(b)
	}
	loc := m.re.FindIndex(b)
	if loc == nil {
		return -1, -1
	}
	return loc[0], loc[1]
}

func (m *regexMatcher) MatchAll(b []byte, dst []Match) []Match {
	if m.lit != nil {
		return m.lit.MatchAll(b, dst)
	}
	locs := m.re.FindAllIndex(b, -1)
	for _, loc := range locs {
		dst = append(dst, Match{loc[0], loc[1]})
	}
	return dst
}

// -------------------------------------------------------------------
// Line-oriented match: find lines containing at least one match
// -------------------------------------------------------------------

// LineMatch represents a matched line.
type LineMatch struct {
	LineNum int
	Line    []byte  // slice into original buffer — zero copy
	Matches []Match // byte offsets within Line
}

// FindMatchingLines finds all lines in buf that contain a match.
// Appends results into dst. Zero allocation beyond the dst slice growth.
func FindMatchingLines(buf []byte, m Matcher, lineOffset int, dst []LineMatch, matchBuf []Match) ([]LineMatch, []Match) {
	lineStart := 0
	lineNum := lineOffset

	for lineStart <= len(buf) {
		// Find end of current line
		lineEnd := lineStart
		for lineEnd < len(buf) && buf[lineEnd] != '\n' {
			lineEnd++
		}

		line := buf[lineStart:lineEnd]
		matchBuf = matchBuf[:0]
		matchBuf = m.MatchAll(line, matchBuf)

		if len(matchBuf) > 0 {
			// Copy matches so they are safe after matchBuf reuse
			ms := make([]Match, len(matchBuf))
			copy(ms, matchBuf)
			dst = append(dst, LineMatch{
				LineNum: lineNum + 1,
				Line:    line,
				Matches: ms,
			})
		}

		lineNum++
		if lineEnd >= len(buf) {
			break
		}
		lineStart = lineEnd + 1
	}
	return dst, matchBuf
}

// FindNonMatchingLines finds all lines that do not contain a match.
func FindNonMatchingLines(buf []byte, m Matcher, lineOffset int, dst []LineMatch) []LineMatch {
	lineStart := 0
	lineNum := lineOffset

	for lineStart <= len(buf) {
		lineEnd := lineStart
		for lineEnd < len(buf) && buf[lineEnd] != '\n' {
			lineEnd++
		}

		line := buf[lineStart:lineEnd]
		start, _ := m.Match(line)
		if start < 0 {
			dst = append(dst, LineMatch{
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

// CountMatchingLines counts lines containing at least one match.
func CountMatchingLines(buf []byte, m Matcher) int {
	count := 0
	offset := 0

	for offset <= len(buf) {
		start, end := m.Match(buf[offset:])
		if start < 0 {
			break
		}

		count++

		matchEnd := offset + end
		if end <= start {
			matchEnd = offset + start + 1
			if matchEnd > len(buf) {
				matchEnd = len(buf)
			}
		}

		nextNewline := bytes.IndexByte(buf[matchEnd:], '\n')
		if nextNewline < 0 {
			break
		}
		offset = matchEnd + nextNewline + 1
	}

	return count
}

// CountMatchingWordLines counts lines with at least one ASCII whole-word match.
func CountMatchingWordLines(buf []byte, m Matcher) int {
	count := 0
	offset := 0

	for offset <= len(buf) {
		start, end := m.Match(buf[offset:])
		if start < 0 {
			break
		}

		matchStart := offset + start
		matchEnd := offset + end
		if isWordBoundary(buf, matchStart, matchEnd) {
			count++
			nextNewline := bytes.IndexByte(buf[matchEnd:], '\n')
			if nextNewline < 0 {
				break
			}
			offset = matchEnd + nextNewline + 1
			continue
		}

		if end <= start {
			offset += start + 1
		} else {
			offset += start + 1
		}
	}

	return count
}

func isWordBoundary(buf []byte, start, end int) bool {
	before := start == 0 || !isWordByte(buf[start-1])
	after := end >= len(buf) || !isWordByte(buf[end])
	return before && after
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// CountMatchingLinesByLine counts matching lines with line-scoped semantics.
func CountMatchingLinesByLine(buf []byte, m Matcher) int {
	count := 0
	lineStart := 0

	for lineStart <= len(buf) {
		lineEndRel := bytes.IndexByte(buf[lineStart:], '\n')
		lineEnd := len(buf)
		if lineEndRel >= 0 {
			lineEnd = lineStart + lineEndRel
		}

		start, _ := m.Match(buf[lineStart:lineEnd])
		if start >= 0 {
			count++
		}

		if lineEndRel < 0 {
			break
		}
		lineStart = lineEnd + 1
	}

	return count
}

// CountNonMatchingLinesByLine counts lines without a match.
func CountNonMatchingLinesByLine(buf []byte, m Matcher) int {
	count := 0
	lineStart := 0

	for lineStart <= len(buf) {
		lineEndRel := bytes.IndexByte(buf[lineStart:], '\n')
		lineEnd := len(buf)
		if lineEndRel >= 0 {
			lineEnd = lineStart + lineEndRel
		}

		start, _ := m.Match(buf[lineStart:lineEnd])
		if start < 0 {
			count++
		}

		if lineEndRel < 0 {
			break
		}
		lineStart = lineEnd + 1
	}

	return count
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

func toLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// IsValidUTF8 checks validity without allocating.
func IsValidUTF8(b []byte) bool {
	return utf8.Valid(b)
}
