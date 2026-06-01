package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const benchPattern = "ZREP_NEEDLE"

type benchCorpus struct {
	dir   string
	bytes int64
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
	}

	for _, op := range ops {
		op := op
		b.Run("zrep/"+op.name, func(b *testing.B) {
			b.SetBytes(corpus.bytes)
			runBenchCommand(b, zrep, op.zrepArgs...)
		})
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

	return benchCorpus{dir: dir, bytes: total}
}
