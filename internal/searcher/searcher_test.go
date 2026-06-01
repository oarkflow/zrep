package searcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zrep/zrep/internal/matcher"
)

func TestSearchFileChunksEmitsBoundedBatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.log")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if _, err := f.WriteString("needle line\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	s := New(matcher.NewLiteral("needle", false))
	var chunks, matches int
	r := s.SearchFileChunks(path, false, func(chunk Result) {
		chunks++
		matches += len(chunk.Matches)
		if len(chunk.Matches) > 256 {
			t.Fatalf("chunk too large: %d", len(chunk.Matches))
		}
	})
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	if chunks != 3 {
		t.Fatalf("expected 3 chunks, got %d", chunks)
	}
	if matches != 600 {
		t.Fatalf("expected 600 matches, got %d", matches)
	}
}

func TestSearchBinaryAsText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, []byte("abc\x00needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(matcher.NewLiteral("needle", false))
	if got := s.SearchFile(path, int64(len("abc\x00needle\n"))); len(got.Matches) != 0 {
		t.Fatalf("expected binary file to be skipped, got %d matches", len(got.Matches))
	}

	s.SearchBinary = true
	got := s.SearchFile(path, int64(len("abc\x00needle\n")))
	if len(got.Matches) != 1 {
		t.Fatalf("expected binary file to be searched as text, got %d matches", len(got.Matches))
	}
}
