package matcher_test

import (
	"bytes"
	"math/rand"
	"testing"

	"github.com/zrep/zrep/internal/matcher"
)

// generateCorpus creates a byte slice of lineCount lines, each lineLen bytes,
// with needle appearing roughly every `freq` lines.
func generateCorpus(lineCount, lineLen, freq int, needle string) []byte {
	var buf bytes.Buffer
	buf.Grow(lineCount * (lineLen + 1))
	r := rand.New(rand.NewSource(42))
	for i := 0; i < lineCount; i++ {
		if freq > 0 && i%freq == 0 {
			buf.WriteString(needle)
			remaining := lineLen - len(needle)
			if remaining > 0 {
				for j := 0; j < remaining; j++ {
					buf.WriteByte(byte('a' + r.Intn(26)))
				}
			}
		} else {
			for j := 0; j < lineLen; j++ {
				buf.WriteByte(byte('a' + r.Intn(26)))
			}
		}
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

var corpus = generateCorpus(100_000, 80, 100, "TODO")

// BenchmarkLiteralMatch measures pure BMH literal matching.
func BenchmarkLiteralMatch(b *testing.B) {
	m := matcher.NewLiteral("TODO", false)
	b.SetBytes(int64(len(corpus)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := make([]matcher.Match, 0, 16)
		dst = m.MatchAll(corpus, dst)
		_ = dst
	}
}

// BenchmarkRegexMatch measures regex matching on the same corpus.
func BenchmarkRegexMatch(b *testing.B) {
	m, _ := matcher.NewRegex("TODO", false)
	b.SetBytes(int64(len(corpus)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := make([]matcher.Match, 0, 16)
		dst = m.MatchAll(corpus, dst)
		_ = dst
	}
}

// BenchmarkFindMatchingLines measures full line-scan + match extraction.
func BenchmarkFindMatchingLines(b *testing.B) {
	m := matcher.NewLiteral("TODO", false)
	b.SetBytes(int64(len(corpus)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lm := make([]matcher.LineMatch, 0, 128)
		mb := make([]matcher.Match, 0, 8)
		lm, _ = matcher.FindMatchingLines(corpus, m, 0, lm, mb)
		_ = lm
	}
}

// BenchmarkCountNewlines measures SWAR newline counting.
func BenchmarkCountNewlines(b *testing.B) {
	b.SetBytes(int64(len(corpus)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := matcher.CountNewlines(corpus)
		_ = n
	}
}

// BenchmarkLiteralCase measures case-insensitive BMH.
func BenchmarkLiteralCase(b *testing.B) {
	m := matcher.NewLiteral("todo", true)
	b.SetBytes(int64(len(corpus)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := make([]matcher.Match, 0, 16)
		dst = m.MatchAll(corpus, dst)
		_ = dst
	}
}

// TestLiteralMatch verifies correctness.
func TestLiteralMatch(t *testing.T) {
	m := matcher.NewLiteral("foo", false)
	b := []byte("hello foo world foo")
	matches := m.MatchAll(b, nil)
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(matches))
	}
	if string(b[matches[0].Start:matches[0].End]) != "foo" {
		t.Errorf("wrong match: %q", b[matches[0].Start:matches[0].End])
	}
}

func TestCaseInsensitive(t *testing.T) {
	m := matcher.NewLiteral("FOO", true)
	b := []byte("hello foo world FOO bar fOo")
	matches := m.MatchAll(b, nil)
	if len(matches) != 3 {
		t.Fatalf("expected 3 matches, got %d: %v", len(matches), matches)
	}
}

func TestNoMatch(t *testing.T) {
	m := matcher.NewLiteral("xyz", false)
	b := []byte("hello world")
	s, e := m.Match(b)
	if s != -1 || e != -1 {
		t.Errorf("expected no match, got %d %d", s, e)
	}
}

func TestCountNewlines(t *testing.T) {
	b := []byte("foo\nbar\nbaz\n")
	n := matcher.CountNewlines(b)
	if n != 3 {
		t.Errorf("expected 3, got %d", n)
	}
}

func TestCountMatchingLines(t *testing.T) {
	m := matcher.NewLiteral("foo", false)
	b := []byte("foo foo\nbar\nbaz foo\nlast")
	n := matcher.CountMatchingLines(b, m)
	if n != 2 {
		t.Errorf("expected 2, got %d", n)
	}
}

func TestCountMatchingLinesByLine(t *testing.T) {
	m, err := matcher.NewRegex("^foo", false)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte("foo one\nnot foo\nfoo two")
	n := matcher.CountMatchingLinesByLine(b, m)
	if n != 2 {
		t.Errorf("expected 2, got %d", n)
	}
}

func TestCountMatchingWordLines(t *testing.T) {
	m := matcher.NewLiteral("foo", false)
	b := []byte("foo\nfood\nbar foo!\npre_foo")
	n := matcher.CountMatchingWordLines(b, m)
	if n != 2 {
		t.Errorf("expected 2, got %d", n)
	}
}

func BenchmarkCountMatchingLines(b *testing.B) {
	m := matcher.NewLiteral("TODO", false)
	b.SetBytes(int64(len(corpus)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := matcher.CountMatchingLines(corpus, m)
		_ = n
	}
}

func BenchmarkCountMatchingLinesByLine(b *testing.B) {
	m, _ := matcher.NewRegex("T[A-Z]+", false)
	b.SetBytes(int64(len(corpus)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := matcher.CountMatchingLinesByLine(corpus, m)
		_ = n
	}
}
