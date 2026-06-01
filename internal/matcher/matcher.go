// Package matcher provides zero-allocation, high-performance pattern matching.
// Uses SWAR (SIMD Within A Register) tricks, Boyer-Moore-Horspool for literals,
// and a hand-tuned NFA/DFA hybrid for regex.
package matcher

import (
	"bytes"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/dlclark/regexp2"
)

// Matcher is the interface all matchers implement.
type Matcher interface {
	// Match returns the first [start, end) byte range in b that matches, or (-1,-1).
	Match(b []byte) (int, int)
	// MatchAll appends all non-overlapping [start,end) match ranges into dst.
	MatchAll(b []byte, dst []Match) []Match
}

type fuzzyMatcher struct {
	needle   string
	distance int
}

type booleanMatcher struct {
	expr boolExpr
}

type boolExpr interface {
	eval(string) bool
	term() string
}

type termExpr string

func (e termExpr) eval(s string) bool { return strings.Contains(s, string(e)) }
func (e termExpr) term() string       { return string(e) }

type notExpr struct{ child boolExpr }

func (e notExpr) eval(s string) bool { return !e.child.eval(s) }
func (e notExpr) term() string       { return e.child.term() }

type binaryExpr struct {
	op          string
	left, right boolExpr
}

func (e binaryExpr) eval(s string) bool {
	if e.op == "AND" {
		return e.left.eval(s) && e.right.eval(s)
	}
	return e.left.eval(s) || e.right.eval(s)
}
func (e binaryExpr) term() string { return e.left.term() }

// NewBoolean creates a simple boolean term matcher supporting AND, OR, NOT,
// parentheses, and quoted terms.
func NewBoolean(pattern string, ignoreCase bool) (Matcher, error) {
	tokens := booleanTokens(pattern)
	p := boolParser{tokens: tokens}
	expr := p.parseOr()
	if expr == nil {
		expr = termExpr(pattern)
	}
	if ignoreCase {
		expr = lowerBoolExpr(expr)
	}
	return &booleanMatcher{expr: expr}, nil
}

func (m *booleanMatcher) Match(b []byte) (int, int) {
	s := strings.ToLower(string(b))
	if !m.expr.eval(s) {
		return -1, -1
	}
	term := m.expr.term()
	if term == "" {
		return 0, 0
	}
	start := strings.Index(s, term)
	if start < 0 {
		return 0, len(b)
	}
	return start, start + len(term)
}

func (m *booleanMatcher) MatchAll(b []byte, dst []Match) []Match {
	start, end := m.Match(b)
	if start >= 0 {
		dst = append(dst, Match{Start: start, End: end})
	}
	return dst
}

type boolParser struct {
	tokens []string
	pos    int
}

func (p *boolParser) parseOr() boolExpr {
	left := p.parseAnd()
	for p.peek("OR") {
		p.pos++
		left = binaryExpr{op: "OR", left: left, right: p.parseAnd()}
	}
	return left
}

func (p *boolParser) parseAnd() boolExpr {
	left := p.parseUnary()
	for p.peek("AND") {
		p.pos++
		left = binaryExpr{op: "AND", left: left, right: p.parseUnary()}
	}
	return left
}

func (p *boolParser) parseUnary() boolExpr {
	if p.peek("NOT") {
		p.pos++
		return notExpr{child: p.parseUnary()}
	}
	if p.peek("(") {
		p.pos++
		expr := p.parseOr()
		if p.peek(")") {
			p.pos++
		}
		return expr
	}
	if p.pos >= len(p.tokens) {
		return termExpr("")
	}
	tok := strings.ToLower(p.tokens[p.pos])
	p.pos++
	return termExpr(tok)
}

func (p *boolParser) peek(tok string) bool {
	return p.pos < len(p.tokens) && strings.EqualFold(p.tokens[p.pos], tok)
}

func booleanTokens(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			if quoted {
				flush()
			}
			quoted = !quoted
		case quoted:
			b.WriteRune(r)
		case r == '(' || r == ')':
			flush()
			out = append(out, string(r))
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

func lowerBoolExpr(expr boolExpr) boolExpr {
	switch e := expr.(type) {
	case termExpr:
		return termExpr(strings.ToLower(string(e)))
	case notExpr:
		return notExpr{child: lowerBoolExpr(e.child)}
	case binaryExpr:
		return binaryExpr{op: e.op, left: lowerBoolExpr(e.left), right: lowerBoolExpr(e.right)}
	default:
		return expr
	}
}

// NewFuzzy creates a bounded edit-distance matcher for whole tokens and lines.
func NewFuzzy(pattern string, distance int) Matcher {
	if distance <= 0 {
		distance = 2
	}
	return &fuzzyMatcher{needle: strings.ToLower(pattern), distance: distance}
}

func (m *fuzzyMatcher) Match(b []byte) (int, int) {
	text := strings.ToLower(string(b))
	bestStart, bestEnd := -1, -1
	for start := 0; start < len(text); {
		for start < len(text) && !isWordByte(text[start]) {
			start++
		}
		end := start
		for end < len(text) && isWordByte(text[end]) {
			end++
		}
		if end > start && editDistanceAtMost(text[start:end], m.needle, m.distance) {
			bestStart, bestEnd = start, end
			break
		}
		if end <= start {
			start++
		} else {
			start = end
		}
	}
	return bestStart, bestEnd
}

func (m *fuzzyMatcher) MatchAll(b []byte, dst []Match) []Match {
	start, end := m.Match(b)
	if start >= 0 {
		dst = append(dst, Match{Start: start, End: end})
	}
	return dst
}

func editDistanceAtMost(a, b string, max int) bool {
	if len(a)-len(b) > max || len(b)-len(a) > max {
		return false
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			cur[j] = minInt(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				cur[j] = minInt(cur[j], prev[j-2]+1)
			}
			if cur[j] < rowMin {
				rowMin = cur[j]
			}
		}
		if rowMin > max {
			return false
		}
		prev, cur = cur, prev
	}
	return prev[len(b)] <= max
}

func minInt(v int, rest ...int) int {
	for _, x := range rest {
		if x < v {
			v = x
		}
	}
	return v
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
	prefix  string // non-empty if regexp has a required literal prefix
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
	prefix, _ := re.LiteralPrefix()

	// Check if it's a pure literal (no metacharacters)
	parsed, _ := syntax.Parse(pattern, syntax.Perl)
	if parsed != nil {
		if literal, ok := literalString(parsed); ok {
			lit := NewLiteral(literal, ignoreCase)
			return &regexMatcher{re: re, literal: literal, prefix: prefix, lit: lit}, nil
		}
	}

	return &regexMatcher{re: re, prefix: prefix}, nil
}

type regexp2Matcher struct {
	re *regexp2.Regexp
}

// NewAdvancedRegex compiles pattern with regexp2. It supports many PCRE-style
// constructs that Go's regexp package intentionally omits, including
// lookaround and backreferences.
func NewAdvancedRegex(pattern string, ignoreCase bool) (Matcher, error) {
	opts := regexp2.RegexOptions(0)
	if ignoreCase {
		opts |= regexp2.IgnoreCase
	}
	re, err := regexp2.Compile(pattern, opts)
	if err != nil {
		return nil, err
	}
	return &regexp2Matcher{re: re}, nil
}

func (m *regexp2Matcher) Match(b []byte) (int, int) {
	text := string(b)
	match, err := m.re.FindStringMatch(text)
	if err != nil || match == nil {
		return -1, -1
	}
	start := len([]byte(text[:match.Index]))
	end := len([]byte(text[:match.Index+match.Length]))
	return start, end
}

func (m *regexp2Matcher) MatchAll(b []byte, dst []Match) []Match {
	text := string(b)
	match, err := m.re.FindStringMatch(text)
	for err == nil && match != nil {
		start := len([]byte(text[:match.Index]))
		end := len([]byte(text[:match.Index+match.Length]))
		dst = append(dst, Match{Start: start, End: end})
		match, err = m.re.FindNextMatch(match)
	}
	return dst
}

// LiteralPrefix returns a required case-sensitive literal prefix when the
// matcher can expose one cheaply.
func LiteralPrefix(m Matcher) (string, bool) {
	rm, ok := m.(*regexMatcher)
	if !ok || rm.prefix == "" {
		return "", false
	}
	return rm.prefix, true
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

func literalString(re *syntax.Regexp) (string, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		return string(re.Rune), true
	case syntax.OpCapture:
		if len(re.Sub) != 1 {
			return "", false
		}
		return literalString(re.Sub[0])
	case syntax.OpConcat:
		var b strings.Builder
		for _, sub := range re.Sub {
			s, ok := literalString(sub)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		}
		return b.String(), true
	}
	return "", false
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
	Context bool    // true when the line is included as surrounding context
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
