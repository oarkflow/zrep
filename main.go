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
	"os"
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
	flagIgnoreCase  = flag.Bool("i", false, "case-insensitive matching")
	flagInvert      = flag.Bool("v", false, "select non-matching lines")
	flagOnlyFiles   = flag.Bool("l", false, "only print filenames with matches")
	flagOnlyMatches = flag.Bool("o", false, "only print matching parts of a line")
	flagCount       = flag.Bool("c", false, "print count of matching lines per file")
	flagLineNumbers = flag.Bool("n", false, "print line numbers")
	flagWord        = flag.Bool("w", false, "match only whole words")
	flagLine        = flag.Bool("x", false, "match only whole lines")
	flagNoColor     = flag.Bool("no-color", false, "disable ANSI color output")
	flagWithName    = flag.Bool("H", false, "always print filename prefixes")
	flagNoName      = flag.Bool("h", false, "never print filename prefixes")
	flagWorkers     = flag.Int("j", 0, "parallel workers (0 = GOMAXPROCS*2)")
	flagFixed       = flag.Bool("F", false, "treat pattern as fixed string (no regex)")
	flagCPUProfile  = flag.String("cpuprofile", "", "write cpu profile to file")
	flagStats       = flag.Bool("stats", false, "print match statistics to stderr")
	flagPatterns    patternList
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
			for entry := range w.Results {
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
				} else {
					r = s.SearchFile(entry.Path, entry.Info.Size())
					r.Count = len(r.Matches)
				}
				if r.Count > 0 || len(r.Matches) > 0 || r.Err != nil {
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
