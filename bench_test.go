package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const benchPattern = "ZREP_NEEDLE"

type benchCorpus struct {
	dir         string
	patternFile string
	gzipFile    string
	csvFile     string
	bytes       int64
}

// BenchmarkZrepVsRipgrep runs an end-to-end CLI benchmark against ripgrep.
//
// Run with:
//
//	go test -run '^$' -bench '^BenchmarkZrepVsRipgrep$' -benchtime=10x
func BenchmarkZrepVsRipgrep(b *testing.B) {
	corpus := createBenchCorpus(b)
	zrep := buildBenchBinary(b)
	rg, rgErr := exec.LookPath("rg")

	ops := []struct {
		name     string
		zrepArgs []string
		rgArgs   []string
	}{
		{
			name:     "literal-count",
			zrepArgs: []string{"-F", "-c", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "literal-files",
			zrepArgs: []string{"-F", "-l", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--files-with-matches", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "literal-print",
			zrepArgs: []string{"-F", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "literal-line-number",
			zrepArgs: []string{"-F", "-n", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--line-number", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "literal-ignore-case-count",
			zrepArgs: []string{"-F", "-i", "-c", "-no-color", "zrep_needle", corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--ignore-case", "--count", "--color=never", "--no-messages", "--no-ignore", "zrep_needle", corpus.dir},
		},
		{
			name:     "regex-count",
			zrepArgs: []string{"-c", "-no-color", `ZREP_[A-Z]+`, corpus.dir},
			rgArgs:   []string{"--count", "--color=never", "--no-messages", "--no-ignore", `ZREP_[A-Z]+`, corpus.dir},
		},
		{
			name:     "only-matching",
			zrepArgs: []string{"-F", "-o", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--only-matching", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "invert-count",
			zrepArgs: []string{"-F", "-v", "-c", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--invert-match", "--count", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "word-count",
			zrepArgs: []string{"-F", "-w", "-c", "-no-color", "alpha", corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--word-regexp", "--count", "--color=never", "--no-messages", "--no-ignore", "alpha", corpus.dir},
		},
		{
			name:     "line-count",
			zrepArgs: []string{"-F", "-x", "-c", "-no-color", "file=001 line=0256 marker=ZREP_NEEDLE alpha beta gamma delta epsilon", corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--line-regexp", "--count", "--color=never", "--no-messages", "--no-ignore", "file=001 line=0256 marker=ZREP_NEEDLE alpha beta gamma delta epsilon", corpus.dir},
		},
		{
			name:     "multi-pattern-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-e", benchPattern, "-e", "theta", corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "-e", benchPattern, "-e", "theta", corpus.dir},
		},
		{
			name:     "include-glob-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-include", "*.log", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--glob", "*.log", benchPattern, corpus.dir},
		},
		{
			name:     "exclude-glob-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-exclude", "*.skip", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--glob", "!*.skip", benchPattern, corpus.dir},
		},
		{
			name:     "include-dir-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-include-dir", "src", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--glob", "**/src/**", benchPattern, corpus.dir},
		},
		{
			name:     "exclude-dir-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-exclude-dir", "generated", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--glob", "!**/generated/**", benchPattern, corpus.dir},
		},
		{
			name:     "hidden-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-hidden", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--hidden", benchPattern, corpus.dir},
		},
		{
			name:     "binary-as-text-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-text", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--text", benchPattern, corpus.dir},
		},
		{
			name:     "files",
			zrepArgs: []string{"--files", "-no-color", corpus.dir},
			rgArgs:   []string{"--files", "--color=never", "--no-messages", "--no-ignore", corpus.dir},
		},
		{
			name:     "pattern-file-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "-f", corpus.patternFile, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "-f", corpus.patternFile, corpus.dir},
		},
		{
			name:     "files-without-match",
			zrepArgs: []string{"-F", "--files-without-match", "-no-color", "NO_SUCH_ZREP_NEEDLE", corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--files-without-match", "--color=never", "--no-messages", "--no-ignore", "NO_SUCH_ZREP_NEEDLE", corpus.dir},
		},
		{
			name:     "count-matches",
			zrepArgs: []string{"-F", "--count-matches", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count-matches", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "max-count",
			zrepArgs: []string{"-F", "--max-count", "1", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--max-count", "1", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "max-columns-preview",
			zrepArgs: []string{"-F", "--max-columns", "48", "--max-columns-preview", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--max-columns", "48", "--max-columns-preview", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "passthru",
			zrepArgs: []string{"-F", "--passthru", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--passthru", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "glob-alias-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "--glob", "*.log", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--glob", "*.log", benchPattern, corpus.dir},
		},
		{
			name:     "sort-path-count",
			zrepArgs: []string{"-F", "-c", "-no-color", "--sort", "path", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--color=never", "--no-messages", "--no-ignore", "--sort", "path", benchPattern, corpus.dir},
		},
		{
			name:     "json-events",
			zrepArgs: []string{"-F", "--json-events", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--json", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "max-filesize-count",
			zrepArgs: []string{"-F", "-c", "--max-filesize", "2M", "-no-color", benchPattern, corpus.dir},
			rgArgs:   []string{"--fixed-strings", "--count", "--max-filesize", "2M", "--color=never", "--no-messages", "--no-ignore", benchPattern, corpus.dir},
		},
		{
			name:     "advanced-regex",
			zrepArgs: []string{"-P", "-c", "-no-color", `(?<=marker=)ZREP_NEEDLE`, corpus.dir},
			rgArgs:   []string{"--pcre2", "--count", "--color=never", "--no-messages", "--no-ignore", `(?<=marker=)ZREP_NEEDLE`, corpus.dir},
		},
		{
			name:     "compressed-gzip",
			zrepArgs: []string{"-F", "--search-compressed", "-c", "-no-color", benchPattern, corpus.gzipFile},
		},
		{
			name:     "fuzzy-count",
			zrepArgs: []string{"--fuzzy", "--distance", "2", "-c", "-no-color", "ZREP_NEEDL", corpus.dir},
		},
		{
			name:     "schema-csv",
			zrepArgs: []string{"--schema", corpus.csvFile},
		},
	}

	for _, op := range ops {
		op := op
		b.Run("zrep/"+op.name, func(b *testing.B) {
			b.SetBytes(corpus.bytes)
			runBenchCommand(b, zrep, op.zrepArgs...)
		})
		if len(op.rgArgs) == 0 {
			continue
		}
		if rgErr != nil {
			b.Logf("rg not found in PATH; skipping ripgrep comparison for %s: %v", op.name, rgErr)
			continue
		}
		b.Run("rg/"+op.name, func(b *testing.B) {
			b.SetBytes(corpus.bytes)
			runBenchCommand(b, rg, op.rgArgs...)
		})
	}
}

func buildBenchBinary(b *testing.B) string {
	b.Helper()

	exe := filepath.Join(b.TempDir(), "zrep")
	cmd := exec.Command("go", "build", "-o", exe, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		b.Fatalf("build zrep benchmark binary: %v\n%s", err, out)
	}
	return exe
}

func runBenchCommand(b *testing.B, exe string, args ...string) {
	b.Helper()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		cmd := exec.Command(exe, args...)
		cmd.Stdout = io.Discard
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		if err := cmd.Run(); err != nil {
			b.Fatalf("%s failed: %v\nstderr:\n%s", exe, err, stderr.String())
		}
	}
}

func createBenchCorpus(tb testing.TB) benchCorpus {
	tb.Helper()

	dir := tb.TempDir()

	const (
		files        = 96
		linesPerFile = 4096
	)

	var total int64
	subdirs := []string{"src", "generated", "logs"}
	for _, subdir := range subdirs {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			tb.Fatalf("create benchmark corpus directory: %v", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755); err != nil {
		tb.Fatalf("create hidden benchmark corpus directory: %v", err)
	}

	for fileIdx := 0; fileIdx < files; fileIdx++ {
		var buf bytes.Buffer
		buf.Grow(linesPerFile * 96)

		for lineIdx := 0; lineIdx < linesPerFile; lineIdx++ {
			if (fileIdx+lineIdx)%257 == 0 {
				fmt.Fprintf(&buf, "file=%03d line=%04d marker=%s alpha beta gamma delta epsilon\n", fileIdx, lineIdx, benchPattern)
				continue
			}
			fmt.Fprintf(&buf, "file=%03d line=%04d alpha beta gamma delta epsilon theta lambda sigma\n", fileIdx, lineIdx)
		}

		subdir := subdirs[fileIdx%len(subdirs)]
		ext := ".txt"
		if fileIdx%4 == 0 {
			ext = ".log"
		}
		if fileIdx%7 == 0 {
			ext = ".skip"
		}
		if fileIdx%31 == 0 {
			subdir = ".hidden"
		}
		path := filepath.Join(dir, subdir, fmt.Sprintf("bench-%03d%s", fileIdx, ext))
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			tb.Fatalf("write benchmark corpus file: %v", err)
		}
		total += int64(buf.Len())
	}

	binaryPath := filepath.Join(dir, "logs", "bench-binary.bin")
	binaryData := append([]byte("prefix\x00"), []byte(benchPattern+"\n")...)
	if err := os.WriteFile(binaryPath, binaryData, 0o644); err != nil {
		tb.Fatalf("write benchmark binary corpus file: %v", err)
	}
	total += int64(len(binaryData))

	patternFile := filepath.Join(dir, "patterns.txt")
	if err := os.WriteFile(patternFile, []byte(benchPattern+"\ntheta\n"), 0o644); err != nil {
		tb.Fatalf("write benchmark pattern file: %v", err)
	}
	gzipFile := filepath.Join(dir, "logs", "bench.gz")
	if err := writeBenchGzip(gzipFile, benchPattern+"\n"); err != nil {
		tb.Fatalf("write benchmark gzip corpus file: %v", err)
	}
	csvFile := filepath.Join(dir, "data.csv")
	if err := os.WriteFile(csvFile, []byte("id,login,note\n1,ada,"+benchPattern+"\n2,bob,plain\n"), 0o644); err != nil {
		tb.Fatalf("write benchmark csv corpus file: %v", err)
	}

	return benchCorpus{dir: dir, patternFile: patternFile, gzipFile: gzipFile, csvFile: csvFile, bytes: total}
}

func writeBenchGzip(path, text string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(text)); err != nil {
		f.Close()
		return err
	}
	if err := gw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
