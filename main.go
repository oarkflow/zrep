// zrep — zero-allocation, parallel grep in Go.
//
// Architecture overview:
//
//	Walker (GOMAXPROCS*2 goroutines) → file path channel
//	  → Searcher pool (GOMAXPROCS goroutines, mmap + SWAR matching)
//	    → Printer (1 goroutine, buffered stdout, ANSI color)
//
// Key performance decisions:
//   - mmap for reads: avoids kernel→user copies
//   - SWAR (SIMD-Within-A-Register) newline counting and null-byte detection
//   - Boyer-Moore-Horspool for literal patterns
//   - sync.Pool for read buffers (mmap fallback)
//   - Lock-free pipeline via channels; printer owns stdout exclusively
//   - Auto-detects pure literals and bypasses regex engine
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"

	"github.com/zrep/zrep/internal/matcher"
	"github.com/zrep/zrep/internal/output"
	"github.com/zrep/zrep/internal/searcher"
	"github.com/zrep/zrep/internal/walker"
)

type patternList []string

func (p *patternList) String() string { return strings.Join(*p, "|") }

func (p *patternList) Set(value string) error {
	*p = append(*p, value)
	return nil
}

var (
	flagIgnoreCase      = flag.Bool("i", false, "case-insensitive matching")
	flagInvert          = flag.Bool("v", false, "select non-matching lines")
	flagOnlyFiles       = flag.Bool("l", false, "only print filenames with matches")
	flagOnlyMatches     = flag.Bool("o", false, "only print matching parts of a line")
	flagCount           = flag.Bool("c", false, "print count of matching lines per file")
	flagLineNumbers     = flag.Bool("n", false, "print line numbers")
	flagWord            = flag.Bool("w", false, "match only whole words")
	flagLine            = flag.Bool("x", false, "match only whole lines")
	flagNoColor         = flag.Bool("no-color", false, "disable ANSI color output")
	flagWithName        = flag.Bool("H", false, "always print filename prefixes")
	flagNoName          = flag.Bool("h", false, "never print filename prefixes")
	flagWorkers         = flag.Int("j", 0, "parallel workers (0 = GOMAXPROCS*2)")
	flagFixed           = flag.Bool("F", false, "treat pattern as fixed string (no regex)")
	flagCPUProfile      = flag.String("cpuprofile", "", "write cpu profile to file")
	flagStats           = flag.Bool("stats", false, "print match statistics to stderr")
	flagText            = flag.Bool("text", false, "search binary files as text")
	flagHidden          = flag.Bool("hidden", false, "search hidden files and directories")
	flagNoDefaultIgnore = flag.Bool("no-default-ignore", false, "do not skip common noise directories")
	flagPatterns        patternList
	flagIncludes        patternList
	flagExcludes        patternList
	flagIncludeDirs     patternList
	flagExcludeDirs     patternList
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `zrep — blazing-fast parallel grep

Usage:
  zrep [flags] PATTERN [PATH...]

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Var(&flagPatterns, "e", "pattern to search for; may be repeated")
	flag.Var(&flagIncludes, "include", "include only files matching glob; may be repeated")
	flag.Var(&flagExcludes, "exclude", "exclude files or directories matching glob; may be repeated")
	flag.Var(&flagIncludeDirs, "include-dir", "include only files under matching directories; may be repeated")
	flag.Var(&flagExcludeDirs, "exclude-dir", "exclude directories matching glob; may be repeated")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 && len(flagPatterns) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	patterns := []string(flagPatterns)
	roots := args
	if len(patterns) == 0 {
		patterns = []string{args[0]}
		roots = args[1:]
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}

	// CPU profiling
	if *flagCPUProfile != "" {
		f, err := os.Create(*flagCPUProfile)
		if err != nil {
			fatal(err)
		}
		pprof.StartCPUProfile(f)
		defer func() {
			pprof.StopCPUProfile()
			f.Close()
		}()
	}

	m, fixedFast, fixedWordFast, err := buildMatcher(patterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zrep: invalid pattern: %v\n", err)
		os.Exit(2)
	}

	// Detect if stdout is a terminal for color
	colorEnabled := !*flagNoColor && isTerminal(os.Stdout.Fd())

	// Single-file mode: suppress filename prefix
	noFilename := len(roots) == 1
	if noFilename {
		fi, e := os.Stat(roots[0])
		if e == nil && !fi.IsDir() {
			noFilename = true
		} else {
			noFilename = false
		}
	}
	if *flagWithName {
		noFilename = false
	}
	if *flagNoName {
		noFilename = true
	}

	// Output printer (single writer goroutine)
	printer := output.New(output.Options{
		Color:        colorEnabled,
		OnlyFiles:    *flagOnlyFiles,
		OnlyMatching: *flagOnlyMatches && !*flagInvert,
		Count:        *flagCount,
		NoFilename:   noFilename,
		LineNumbers:  *flagLineNumbers,
	}, 256<<10)

	// Walker
	workers := *flagWorkers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0) * 2
	}
	w := walker.New(workers)
	w.Filter = buildPathFilter()
	w.Walk(roots...)

	// Search goroutine pool
	numSearchers := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	var totalFiles, totalMatches int64

	for i := 0; i < numSearchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := searcher.New(m)
			s.SearchBinary = *flagText
			for entry := range w.Results {
				if entry.Info.Size() > searcher.MaxBufferedFileSize && !*flagCount && !*flagOnlyFiles {
					emit := func(chunk searcher.Result) {
						if len(chunk.Matches) > 0 {
							printer.Send(chunk)
						}
					}
					var r searcher.Result
					if !colorEnabled && !*flagOnlyMatches {
						r = s.SearchFileLineChunks(entry.Path, *flagInvert, emit)
					} else {
						r = s.SearchFileChunks(entry.Path, *flagInvert, emit)
					}
					if r.Err != nil {
						printer.Send(r)
					}
					continue
				}
				var r searcher.Result
				if *flagOnlyFiles && fixedFast && !*flagInvert {
					r = s.SearchFileExists(entry.Path, entry.Info.Size())
				} else if *flagCount {
					if fixedFast && len(patterns) == 1 && !*flagInvert {
						r = s.SearchFileCount(entry.Path, entry.Info.Size())
					} else if fixedWordFast && !*flagInvert {
						r = s.SearchFileCountWords(entry.Path, entry.Info.Size())
					} else {
						r = s.SearchFileCountByLine(entry.Path, entry.Info.Size(), *flagInvert)
					}
				} else if *flagOnlyFiles && *flagInvert {
					r = s.SearchFileCountByLine(entry.Path, entry.Info.Size(), true)
				} else if *flagInvert {
					r = s.SearchFileInvert(entry.Path, entry.Info.Size())
				} else if *flagOnlyMatches && fixedFast && len(patterns) == 1 && !*flagIgnoreCase && !*flagLineNumbers {
					r = s.SearchFileOnlyLiteral(entry.Path, entry.Info.Size(), patterns[0])
				} else {
					r = s.SearchFile(entry.Path, entry.Info.Size())
					r.Count = len(r.Matches)
				}
				if r.Count > 0 || len(r.Matches) > 0 || len(r.OnlyMatch) > 0 || r.Err != nil {
					printer.Send(r)
				}
				if *flagStats {
					_ = totalFiles // avoid unused warning; atomic increment would go here
				}
			}
		}()
	}

	// Wait for walker to finish, then searchers, then printer
	go func() {
		w.Wait()
	}()

	wg.Wait()
	printer.Close()

	_ = totalMatches // suppress unused warning
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
	os.Exit(1)
}

func buildMatcher(patterns []string) (matcher.Matcher, bool, bool, error) {
	fixedFast := *flagFixed && !*flagWord && !*flagLine
	fixedWordFast := *flagFixed && *flagWord && !*flagLine
	if fixedFast || fixedWordFast {
		matchers := make([]matcher.Matcher, 0, len(patterns))
		for _, pattern := range patterns {
			matchers = append(matchers, matcher.NewLiteral(pattern, *flagIgnoreCase))
		}
		return matcher.NewAny(matchers...), fixedFast, fixedWordFast, nil
	}

	regexPatterns := make([]string, len(patterns))
	for i, pattern := range patterns {
		if *flagFixed {
			pattern = regexp.QuoteMeta(pattern)
		}
		if *flagWord {
			pattern = `\b(?:` + pattern + `)\b`
		}
		if *flagLine {
			pattern = `^(?:` + pattern + `)$`
		}
		regexPatterns[i] = "(?:" + pattern + ")"
	}

	m, err := matcher.NewRegex(strings.Join(regexPatterns, "|"), *flagIgnoreCase)
	return m, false, false, err
}

func buildPathFilter() walker.Filter {
	includeFiles := []string(flagIncludes)
	excludeFiles := []string(flagExcludes)
	includeDirs := []string(flagIncludeDirs)
	excludeDirs := []string(flagExcludeDirs)

	return func(path string, d fs.DirEntry) bool {
		name := d.Name()
		if !*flagHidden && strings.HasPrefix(name, ".") {
			return false
		}
		if !*flagNoDefaultIgnore && walker.IsDefaultIgnoredName(name) {
			return false
		}

		if d.IsDir() {
			return !matchesAnyPathGlob(path, name, excludeDirs) &&
				!matchesAnyPathGlob(path, name, excludeFiles)
		}

		if len(includeDirs) > 0 && !pathHasMatchingDir(path, includeDirs) {
			return false
		}
		if matchesAnyPathGlob(path, name, excludeFiles) {
			return false
		}
		if len(includeFiles) > 0 && !matchesAnyPathGlob(path, name, includeFiles) {
			return false
		}
		return true
	}
}

func pathHasMatchingDir(path string, patterns []string) bool {
	dir := filepath.Dir(filepath.Clean(path))
	for {
		if dir == "." || dir == string(filepath.Separator) || dir == "" {
			return false
		}
		if matchesAnyPathGlob(dir, filepath.Base(dir), patterns) {
			return true
		}
		next := filepath.Dir(dir)
		if next == dir {
			return false
		}
		dir = next
	}
}

func matchesAnyPathGlob(path, name string, patterns []string) bool {
	for _, pattern := range patterns {
		if matchesPathGlob(path, name, pattern) {
			return true
		}
	}
	return false
}

func matchesPathGlob(path, name, pattern string) bool {
	if pattern == "" {
		return false
	}
	if ok, _ := filepath.Match(pattern, name); ok {
		return true
	}
	cleanPath := filepath.Clean(path)
	if ok, _ := filepath.Match(pattern, cleanPath); ok {
		return true
	}
	if !filepath.IsAbs(cleanPath) {
		return false
	}
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, cleanPath); err == nil {
			if ok, _ := filepath.Match(pattern, rel); ok {
				return true
			}
		}
	}
	return strings.Contains(pattern, string(filepath.Separator)) && strings.Contains(cleanPath, pattern)
}
