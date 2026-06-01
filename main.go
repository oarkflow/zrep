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
	"bufio"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	flagIgnoreCase        = flag.Bool("i", false, "case-insensitive matching")
	flagSmartCase         = flag.Bool("S", false, "smart-case matching (ignore case unless pattern has uppercase)")
	flagInvert            = flag.Bool("v", false, "select non-matching lines")
	flagOnlyFiles         = flag.Bool("l", false, "only print filenames with matches")
	flagOnlyMatches       = flag.Bool("o", false, "only print matching parts of a line")
	flagCount             = flag.Bool("c", false, "print count of matching lines per file")
	flagLineNumbers       = flag.Bool("n", false, "print line numbers")
	flagWord              = flag.Bool("w", false, "match only whole words")
	flagLine              = flag.Bool("x", false, "match only whole lines")
	flagNoColor           = flag.Bool("no-color", false, "disable ANSI color output")
	flagWithName          = flag.Bool("H", false, "always print filename prefixes")
	flagNoName            = flag.Bool("h", false, "never print filename prefixes")
	flagWorkers           = flag.Int("j", 0, "parallel workers (0 = GOMAXPROCS*2)")
	flagFixed             = flag.Bool("F", false, "treat pattern as fixed string (no regex)")
	flagCPUProfile        = flag.String("cpuprofile", "", "write cpu profile to file")
	flagStats             = flag.Bool("stats", false, "print match statistics to stderr")
	flagNoIgnore          = flag.Bool("no-ignore", false, "do not read .gitignore, .ignore, or .zrepignore")
	flagFiles             = flag.Bool("files", false, "print files that would be searched")
	flagFilesWithout      = flag.Bool("files-without-match", false, "print files with zero matches")
	flagCountMatches      = flag.Bool("count-matches", false, "count individual matches instead of matching lines")
	flagIncludeZero       = flag.Bool("include-zero", false, "print zero counts in count modes")
	flagQuiet             = flag.Bool("q", false, "suppress stdout and stop after first match")
	flagMaxCount          = flag.Int("m", 0, "stop after NUM matching lines per file")
	flagPassthru          = flag.Bool("passthru", false, "print matching and non-matching lines")
	flagTrim              = flag.Bool("trim", false, "trim leading whitespace in printed lines")
	flagMaxColumns        = flag.Int("max-columns", 0, "omit matching lines longer than NUM bytes")
	flagMaxColumnsPreview = flag.Bool("max-columns-preview", false, "preview lines longer than --max-columns")
	flagNull              = flag.Bool("0", false, "terminate path records with NUL")
	flagPathSeparator     = flag.String("path-separator", "", "rewrite printed path separators")
	flagNoMessages        = flag.Bool("no-messages", false, "suppress file read/open errors")
	flagDebug             = flag.Bool("debug", false, "print debug messages to stderr")
	flagVersion           = flag.Bool("version", false, "print version and exit")
	flagNoConfig          = flag.Bool("no-config", false, "do not read zrep config files")
	flagJSONEvents        = flag.Bool("json-events", false, "print ripgrep-style JSON event records")
	flagSort              = flag.String("sort", "", "sort by path, modified, or size")
	flagSortReverse       = flag.String("sortr", "", "reverse sort by path, modified, or size")
	flagReplace           = flag.String("replace", "", "print matches with replacement text applied")
	flagWrite             = flag.Bool("write", false, "write --replace changes back to regular files")
	flagMultiline         = flag.Bool("multiline", false, "allow matches to span multiple lines")
	flagSearchArchives    = flag.Bool("search-archives", false, "search .gz and .zip archive contents")
	flagEncoding          = flag.String("encoding", "auto", "text encoding: auto, utf-8, utf-16le, utf-16be")
	flagSample            = flag.Int("sample", 0, "print first NUM rows/records from each file")
	flagFormat            = flag.String("format", "plain", "inspect output format: plain, json, pretty-json, table, csv")
	flagMaxCellWidth      = flag.Int("max-cell-width", 80, "maximum table cell width before truncating (0 = no limit)")
	flagLimit             = flag.Int("limit", 0, "alias for -sample in inspect mode")
	flagSelect            = flag.String("select", "", "inspect fields/columns to display")
	flagWhere             = flag.String("where", "", "inspect filter FIELD=VALUE")
	flagFlatten           = flag.Bool("flatten", false, "flatten nested JSON fields in inspect output")
	flagRows              = flag.Bool("rows", false, "print total rows/records per file")
	flagColumns           = flag.Bool("columns", false, "print CSV columns or JSON object keys")
	flagInspect           = flag.Bool("inspect", false, "print file kind, rows, columns, and sample metadata")
	flagAfterContext      = flag.Int("A", 0, "print NUM lines of trailing context")
	flagBeforeContext     = flag.Int("B", 0, "print NUM lines of leading context")
	flagContext           = flag.Int("C", 0, "print NUM lines of leading and trailing context")
	flagJSON              = flag.Bool("json", false, "print newline-delimited JSON results")
	flagVimgrep           = flag.Bool("vimgrep", false, "print file:line:column:text results")
	flagHeading           = flag.Bool("heading", false, "group normal matches under file headings")
	flagPretty            = flag.Bool("pretty", false, "print pretty block output (default)")
	flagText              = flag.Bool("text", false, "search binary files as text")
	flagHidden            = flag.Bool("hidden", false, "search hidden files and directories")
	flagNoDefaultIgnore   = flag.Bool("no-default-ignore", false, "do not skip common noise directories")
	flagTypeList          = flag.Bool("type-list", false, "list supported file types")
	flagPatterns          patternList
	flagConfigFiles       patternList
	flagPatternFiles      patternList
	flagGlobs             patternList
	flagIncludes          patternList
	flagExcludes          patternList
	flagIncludeDirs       patternList
	flagExcludeDirs       patternList
	flagTypes             patternList
	flagTypeExcludes      patternList
	flagTypeAdds          patternList
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
	flag.Var(&flagConfigFiles, "config", "read flags from config file")
	flag.Var(&flagPatternFiles, "f", "read patterns from file; may be repeated")
	flag.Var(&flagPatternFiles, "file", "read patterns from file; may be repeated")
	flag.Var(&flagGlobs, "g", "include glob, or exclude with !GLOB; may be repeated")
	flag.Var(&flagGlobs, "glob", "include glob, or exclude with !GLOB; may be repeated")
	flag.Var(&flagIncludes, "include", "include only files matching glob; may be repeated")
	flag.Var(&flagExcludes, "exclude", "exclude files or directories matching glob; may be repeated")
	flag.Var(&flagIncludeDirs, "include-dir", "include only files under matching directories; may be repeated")
	flag.Var(&flagExcludeDirs, "exclude-dir", "exclude directories matching glob; may be repeated")
	flag.Var(&flagTypes, "t", "include files of type (go, js, ts, py, rust, java, c, cpp, md, json); may be repeated")
	flag.Var(&flagTypeExcludes, "T", "exclude files of type; may be repeated")
	flag.Var(&flagTypeAdds, "type-add", "add file type as name:glob; may be repeated")
	registerShortAliases()
	var err error
	os.Args, err = expandConfigArgs(os.Args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
		os.Exit(2)
	}
	os.Args = expandShortFlagClusters(os.Args)
	os.Args = reorderFlagsBeforePositionals(os.Args)
	flag.Parse()
	started := time.Now()

	args := flag.Args()
	if *flagVersion {
		fmt.Println("zrep dev")
		return
	}
	if *flagTypeList {
		printTypeList()
		return
	}
	if hasInspectOperation() {
		if *flagSample == 0 && flagLookupChanged("limit") {
			*flagSample = *flagLimit
		}
		roots := args
		if len(roots) == 0 {
			roots = []string{"."}
		}
		runInspect(roots)
		return
	}
	if *flagFiles {
		roots := args
		if len(roots) == 0 {
			roots = []string{"."}
		}
		runListFiles(roots)
		return
	}
	if err := loadPatternFiles(); err != nil {
		fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
		os.Exit(2)
	}
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
		if stdinIsPipe() {
			roots = []string{"-"}
		} else {
			roots = []string{"."}
		}
	}
	if *flagSmartCase && !*flagIgnoreCase && allLowerPatterns(patterns) {
		*flagIgnoreCase = true
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
		Color:             colorEnabled,
		OnlyFiles:         *flagOnlyFiles || *flagFilesWithout,
		OnlyMatching:      *flagOnlyMatches && !*flagInvert,
		Count:             *flagCount || *flagCountMatches,
		NoFilename:        noFilename,
		LineNumbers:       *flagLineNumbers,
		Mode:              outputMode(),
		Heading:           *flagHeading,
		Replace:           *flagReplace,
		IncludeZero:       *flagIncludeZero,
		Null:              *flagNull,
		PathSeparator:     *flagPathSeparator,
		Trim:              *flagTrim,
		MaxColumns:        *flagMaxColumns,
		MaxColumnsPreview: *flagMaxColumnsPreview,
		NoMessages:        *flagNoMessages,
		Quiet:             *flagQuiet,
	}, 256<<10)

	workers := *flagWorkers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0) * 2
	}

	numSearchers := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	var totalFiles, totalMatches, totalBytes, totalSkipped int64
	var hadMatch, hadErr atomic.Bool
	before, after := contextLines()
	searchOne := func(entry walker.Entry) {
		atomic.AddInt64(&totalFiles, 1)
		atomic.AddInt64(&totalBytes, entry.Info.Size())
		s := searcher.New(m)
		s.SearchBinary = *flagText || *flagPassthru
		s.Encoding = *flagEncoding
		if *flagSearchArchives && searcher.IsSupportedArchive(entry.Path) {
			matchedAny := false
			for _, r := range s.SearchArchive(entry.Path) {
				matched := resultMatches(r) > 0 || len(r.Matches) > 0 || len(r.OnlyMatch) > 0
				if r.Err != nil {
					hadErr.Store(true)
				}
				if matched {
					matchedAny = true
					hadMatch.Store(true)
				}
				if r.Err != nil || matched {
					atomic.AddInt64(&totalMatches, resultMatches(r))
					printer.Send(r)
				}
			}
			if *flagFilesWithout && !matchedAny {
				printer.Send(searcher.Result{Path: entry.Path, Count: 1, Force: true})
			}
			return
		}
		r := searchPath(s, entry.Path, entry.Info.Size(), patterns, fixedFast, fixedWordFast, before, after)
		matched := resultMatches(r) > 0 || len(r.Matches) > 0 || len(r.OnlyMatch) > 0
		if *flagFilesWithout {
			if !matched && r.Err == nil {
				r = searcher.Result{Path: entry.Path, Count: 1, Force: true}
				printer.Send(r)
			}
			return
		}
		if r.Err != nil {
			hadErr.Store(true)
		}
		if matched {
			hadMatch.Store(true)
		}
		if r.Force || r.Err != nil || matched || ((*flagCount || *flagCountMatches) && *flagIncludeZero) {
			if (*flagCount || *flagCountMatches) && *flagIncludeZero {
				r.Force = true
			}
			atomic.AddInt64(&totalMatches, resultMatches(r))
			printer.Send(r)
		}
	}

	if len(roots) == 1 && roots[0] == "-" {
		s := searcher.New(m)
		s.SearchBinary = true
		s.Encoding = *flagEncoding
		r := s.SearchReader("-", os.Stdin, searchOptions())
		if *flagCountMatches {
			r.Count = countResultMatches(r)
		}
		if len(r.Matches) > 0 || r.Count > 0 {
			hadMatch.Store(true)
		}
		if r.Err != nil {
			hadErr.Store(true)
		}
		if r.Err != nil || len(r.Matches) > 0 || r.Count > 0 || ((*flagCount || *flagCountMatches) && *flagIncludeZero) {
			r.Force = (*flagCount || *flagCountMatches) && *flagIncludeZero
			printer.Send(r)
		}
		printer.Close()
		printJSONSummary(totalFiles, totalBytes, totalMatches, started)
		exitForSearch(hadMatch.Load(), hadErr.Load())
		return
	}

	if sortMode() != "" {
		entries := collectEntries(roots, workers)
		sortEntries(entries, sortMode(), *flagSortReverse != "")
		for _, entry := range entries {
			searchOne(entry)
			if *flagQuiet && hadMatch.Load() {
				break
			}
		}
		printer.Close()
		printJSONSummary(totalFiles, totalBytes, totalMatches, started)
		printStats(totalFiles, totalBytes, totalMatches, totalSkipped, started)
		exitForSearch(hadMatch.Load(), hadErr.Load())
		return
	}

	w := walker.New(workers)
	w.Filter = buildPathFilter()
	w.Walk(roots...)

	for i := 0; i < numSearchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range w.Results {
				if *flagQuiet && hadMatch.Load() {
					continue
				}
				searchOne(entry)
			}
		}()
	}

	// Wait for walker to finish, then searchers, then printer
	go func() {
		w.Wait()
	}()

	wg.Wait()
	printer.Close()

	printJSONSummary(totalFiles, totalBytes, totalMatches, started)
	printStats(totalFiles, totalBytes, totalMatches, totalSkipped, started)
	exitForSearch(hadMatch.Load(), hadErr.Load())
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
	os.Exit(1)
}

func debugf(format string, args ...any) {
	if *flagDebug {
		fmt.Fprintf(os.Stderr, "zrep: debug: "+format+"\n", args...)
	}
}

func expandConfigArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	if configDisabled(args) {
		return args, nil
	}
	configPaths := defaultConfigPaths()
	configPaths = append(configPaths, explicitConfigPaths(args)...)
	if len(configPaths) == 0 {
		return args, nil
	}
	configArgs := make([]string, 0)
	seen := map[string]bool{}
	for _, path := range configPaths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		tokens, err := readConfigArgs(path)
		if err != nil {
			if isDefaultConfigPath(path) && os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		configArgs = append(configArgs, tokens...)
	}
	if len(configArgs) == 0 {
		return args, nil
	}
	out := make([]string, 0, len(args)+len(configArgs))
	out = append(out, args[0])
	out = append(out, configArgs...)
	out = append(out, args[1:]...)
	return out, nil
}

func configDisabled(args []string) bool {
	if envTruthy(os.Getenv("ZREP_NO_CONFIG")) {
		return true
	}
	for _, arg := range args[1:] {
		if arg == "--no-config" || arg == "-no-config" {
			return true
		}
	}
	return false
}

func envTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func defaultConfigPaths() []string {
	if path := os.Getenv("ZREP_CONFIG_PATH"); path != "" {
		return splitConfigPathList(path)
	}
	return nil
}

func splitConfigPathList(value string) []string {
	parts := strings.Split(value, string(os.PathListSeparator))
	out := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func explicitConfigPaths(args []string) []string {
	var paths []string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--config" || arg == "-config" {
			if i+1 < len(args) {
				paths = append(paths, args[i+1])
				i++
			}
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--config="); ok {
			paths = append(paths, value)
			continue
		}
		if value, ok := strings.CutPrefix(arg, "-config="); ok {
			paths = append(paths, value)
		}
	}
	return paths
}

func isDefaultConfigPath(path string) bool {
	for _, candidate := range defaultConfigPaths() {
		if candidate == path {
			return true
		}
	}
	return false
}

func readConfigArgs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var args []string
	sc := bufio.NewScanner(f)
	lineNum := 0
	for sc.Scan() {
		lineNum++
		tokens, err := splitConfigLine(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNum, err)
		}
		args = append(args, tokens...)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return args, nil
}

func splitConfigLine(line string) ([]string, error) {
	var tokens []string
	var b strings.Builder
	var quote rune
	escaped := false
	inToken := false
	for _, r := range line {
		if escaped {
			b.WriteRune(r)
			inToken = true
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			inToken = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			inToken = true
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			inToken = true
		case '#':
			if !inToken {
				return tokens, nil
			}
			b.WriteRune(r)
		case ' ', '\t', '\r', '\n':
			if inToken {
				tokens = append(tokens, b.String())
				b.Reset()
				inToken = false
			}
		default:
			b.WriteRune(r)
			inToken = true
		}
	}
	if escaped {
		b.WriteRune('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inToken {
		tokens = append(tokens, b.String())
	}
	return tokens, nil
}

func loadPatternFiles() error {
	for _, file := range flagPatternFiles {
		var r *bufio.Scanner
		if file == "-" {
			r = bufio.NewScanner(os.Stdin)
		} else {
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()
			r = bufio.NewScanner(f)
		}
		for r.Scan() {
			flagPatterns = append(flagPatterns, r.Text())
		}
		if err := r.Err(); err != nil {
			return err
		}
	}
	return nil
}

func flagLookupChanged(name string) bool {
	changed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			changed = true
		}
	})
	return changed
}

func stdinIsPipe() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0
}

func searchOptions() searcher.Options {
	return searcher.Options{
		Invert:   *flagInvert,
		Passthru: *flagPassthru,
		MaxCount: *flagMaxCount,
	}
}

func searchPath(s *searcher.Searcher, path string, size int64, patterns []string, fixedFast, fixedWordFast bool, before, after int) searcher.Result {
	if *flagWrite && *flagReplace != "" {
		return s.ReplaceFile(path, size, *flagReplace)
	}
	if *flagSearchArchives && searcher.IsSupportedArchive(path) {
		results := s.SearchArchive(path)
		total := searcher.Result{Path: path}
		for _, r := range results {
			total.Count += int(resultMatches(r))
			total.Matches = append(total.Matches, r.Matches...)
			if r.Err != nil {
				total.Err = r.Err
			}
		}
		return total
	}
	if *flagFilesWithout && fixedFast && !*flagInvert {
		return s.SearchFileExists(path, size)
	}
	if *flagCountMatches {
		if fixedFast && len(patterns) == 1 && !*flagIgnoreCase && !*flagInvert {
			return s.SearchFileCountLiteralMatches(path, size, patterns[0])
		}
		return s.SearchFileCountMatches(path, size)
	}
	if *flagPassthru || *flagMaxCount > 0 {
		r := s.SearchFileOptions(path, size, searchOptions())
		if *flagCount {
			r.Matches = nil
		}
		return r
	}
	if *flagOnlyFiles && fixedFast && !*flagInvert {
		return s.SearchFileExists(path, size)
	}
	if *flagCount {
		if *flagFixed && *flagLine && !*flagIgnoreCase && len(patterns) == 1 && !*flagInvert {
			return s.SearchFileCountExactLine(path, size, patterns[0])
		}
		if fixedFast && len(patterns) == 1 && !*flagInvert {
			return s.SearchFileCount(path, size)
		}
		if fixedWordFast && !*flagInvert {
			return s.SearchFileCountWords(path, size)
		}
		return s.SearchFileCountByLine(path, size, *flagInvert)
	}
	if *flagOnlyFiles && *flagInvert {
		return s.SearchFileCountByLine(path, size, true)
	}
	if *flagInvert {
		return s.SearchFileInvert(path, size)
	}
	if outputMode() == "" && *flagOnlyMatches && fixedFast && len(patterns) == 1 && !*flagIgnoreCase && !*flagLineNumbers {
		return s.SearchFileOnlyLiteral(path, size, patterns[0])
	}
	if before > 0 || after > 0 {
		return s.SearchFileContext(path, size, before, after)
	}
	if *flagMultiline {
		return s.SearchFileMultiline(path, size)
	}
	r := s.SearchFile(path, size)
	r.Count = len(r.Matches)
	return r
}

func countResultMatches(r searcher.Result) int {
	n := 0
	for _, lm := range r.Matches {
		n += len(lm.Matches)
	}
	return n
}

func runListFiles(roots []string) {
	workers := *flagWorkers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0) * 2
	}
	entries := collectEntries(roots, workers)
	sortEntries(entries, sortMode(), *flagSortReverse != "")
	bw := bufio.NewWriter(os.Stdout)
	defer bw.Flush()
	for _, entry := range entries {
		path := formatOutputPath(entry.Path)
		bw.WriteString(path)
		if *flagNull {
			bw.WriteByte(0)
		} else {
			bw.WriteByte('\n')
		}
	}
}

func collectEntries(roots []string, workers int) []walker.Entry {
	w := walker.New(workers)
	w.Filter = buildPathFilter()
	w.Walk(roots...)
	go w.Wait()
	entries := make([]walker.Entry, 0, 1024)
	for entry := range w.Results {
		entries = append(entries, entry)
	}
	return entries
}

func sortMode() string {
	if *flagSortReverse != "" {
		return strings.ToLower(*flagSortReverse)
	}
	return strings.ToLower(*flagSort)
}

func sortEntries(entries []walker.Entry, mode string, reverse bool) {
	if mode == "" || mode == "none" {
		return
	}
	less := func(i, j int) bool { return entries[i].Path < entries[j].Path }
	switch mode {
	case "path":
		less = func(i, j int) bool { return entries[i].Path < entries[j].Path }
	case "modified":
		less = func(i, j int) bool { return entries[i].Info.ModTime().Before(entries[j].Info.ModTime()) }
	case "size":
		less = func(i, j int) bool { return entries[i].Info.Size() < entries[j].Info.Size() }
	default:
		fatal(fmt.Errorf("unknown sort mode %q", mode))
	}
	sort.Slice(entries, func(i, j int) bool {
		if reverse {
			return less(j, i)
		}
		return less(i, j)
	})
}

func formatOutputPath(path string) string {
	if *flagPathSeparator == "" {
		return path
	}
	path = strings.ReplaceAll(path, "/", *flagPathSeparator)
	return strings.ReplaceAll(path, "\\", *flagPathSeparator)
}

func printStats(totalFiles, totalBytes, totalMatches, totalSkipped int64, started time.Time) {
	if !*flagStats {
		return
	}
	fmt.Fprintf(os.Stderr, "zrep: files=%d skipped=%d bytes=%d matches=%d elapsed=%s mode=%s\n",
		atomic.LoadInt64(&totalFiles),
		atomic.LoadInt64(&totalSkipped),
		atomic.LoadInt64(&totalBytes),
		atomic.LoadInt64(&totalMatches),
		time.Since(started).Round(time.Millisecond),
		outputMode())
}

func printJSONSummary(totalFiles, totalBytes, totalMatches int64, started time.Time) {
	if !*flagJSONEvents || *flagQuiet {
		return
	}
	fmt.Printf("{\"type\":\"summary\",\"data\":{\"files\":%d,\"bytes\":%d,\"matches\":%d,\"elapsed_ms\":%d}}\n",
		atomic.LoadInt64(&totalFiles),
		atomic.LoadInt64(&totalBytes),
		atomic.LoadInt64(&totalMatches),
		time.Since(started).Milliseconds())
}

func exitForSearch(matched, failed bool) {
	if failed {
		os.Exit(2)
	}
	if *flagQuiet {
		if matched {
			os.Exit(0)
		}
		os.Exit(1)
	}
}

func registerShortAliases() {
	flag.BoolVar(flagQuiet, "quiet", false, "alias for -q")
	flag.IntVar(flagMaxCount, "max-count", 0, "alias for -m")
	flag.BoolVar(flagFiles, "list-files", false, "alias for -files")
	flag.BoolVar(flagFilesWithout, "without-match", false, "alias for -files-without-match")
	flag.BoolVar(flagCountMatches, "matches-count", false, "alias for -count-matches")
	flag.BoolVar(flagPassthru, "all-lines", false, "alias for -passthru")
	flag.IntVar(flagMaxCellWidth, "cell-width", 80, "alias for -max-cell-width")
	flag.BoolVar(flagStats, "s", false, "alias for -stats")
	flag.BoolVar(flagJSON, "J", false, "alias for -json")
	flag.BoolVar(flagVimgrep, "G", false, "alias for -vimgrep")
	flag.BoolVar(flagPretty, "P", false, "alias for -pretty")
	flag.BoolVar(flagInspect, "I", false, "alias for -inspect")
	flag.BoolVar(flagRows, "R", false, "alias for -rows")
	flag.BoolVar(flagColumns, "K", false, "alias for -columns")
	flag.BoolVar(flagMultiline, "M", false, "alias for -multiline")
	flag.BoolVar(flagSearchArchives, "Z", false, "alias for -search-archives")
	flag.BoolVar(flagWrite, "W", false, "alias for -write")
	flag.BoolVar(flagTypeList, "Y", false, "alias for -type-list")
	flag.StringVar(flagReplace, "r", "", "alias for -replace")
	flag.StringVar(flagEncoding, "E", "auto", "alias for -encoding")
	flag.IntVar(flagSample, "N", 0, "alias for -sample")
	flag.IntVar(flagLimit, "L", 0, "alias for -limit")
}

func expandShortFlagClusters(args []string) []string {
	if len(args) == 0 {
		return args
	}
	out := make([]string, 0, len(args))
	out = append(out, args[0])
	boolFlags := shortClusterBoolFlags()
	for _, arg := range args[1:] {
		if !isShortCluster(arg, boolFlags) {
			out = append(out, arg)
			continue
		}
		for _, ch := range arg[1:] {
			name := string(ch)
			if name == "j" {
				name = "J"
			} else if name == "r" {
				name = "R"
			}
			out = append(out, "-"+name)
		}
	}
	return out
}

func reorderFlagsBeforePositionals(args []string) []string {
	if len(args) == 0 {
		return args
	}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	flags = append(flags, args[0])
	valueFlags := flagsWithValues()

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !looksLikeFlag(arg) {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		if flagNeedsValue(arg, valueFlags) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positionals...)
}

func looksLikeFlag(arg string) bool {
	return len(arg) > 1 && strings.HasPrefix(arg, "-") && arg != "-"
}

func flagNeedsValue(arg string, valueFlags map[string]bool) bool {
	name := strings.TrimLeft(arg, "-")
	if name == "" || strings.Contains(name, "=") {
		return false
	}
	return valueFlags[name]
}

func flagsWithValues() map[string]bool {
	return map[string]bool{
		"A": true, "B": true, "C": true, "E": true, "L": true, "N": true,
		"T": true, "e": true, "f": true, "g": true, "j": true, "m": true, "r": true, "t": true,
		"cell-width": true,
		"config":     true, "cpuprofile": true, "encoding": true, "exclude": true,
		"exclude-dir": true, "file": true, "format": true, "glob": true, "include": true,
		"include-dir": true, "limit": true, "max-columns": true,
		"max-cell-width": true, "max-count": true, "path-separator": true, "replace": true,
		"sample": true, "select": true, "sort": true, "sortr": true,
		"type-add": true, "where": true,
	}
}

func isShortCluster(arg string, boolFlags map[rune]bool) bool {
	if len(arg) <= 2 || !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") || strings.Contains(arg, "=") {
		return false
	}
	for _, ch := range arg[1:] {
		if !boolFlags[ch] {
			return false
		}
	}
	return true
}

func shortClusterBoolFlags() map[rune]bool {
	return map[rune]bool{
		'F': true, 'G': true, 'H': true, 'I': true, 'J': true, 'K': true,
		'M': true, 'P': true, 'R': true, 'S': true, 'W': true, 'Y': true,
		'Z': true, 'c': true, 'h': true, 'i': true, 'j': true, 'l': true,
		'n': true, 'o': true, 'q': true, 'r': true, 's': true, 'v': true, 'w': true, 'x': true,
	}
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

func outputMode() string {
	if *flagJSONEvents {
		return "json-events"
	}
	if *flagJSON {
		return "json"
	}
	if *flagVimgrep {
		return "vimgrep"
	}
	return ""
}

func hasInspectOperation() bool {
	return *flagInspect || *flagRows || *flagColumns || *flagSample > 0 || *flagLimit > 0 || *flagSelect != "" || *flagWhere != "" || *flagFlatten
}

func contextLines() (int, int) {
	before, after := *flagBeforeContext, *flagAfterContext
	if *flagContext > 0 {
		before, after = *flagContext, *flagContext
	}
	return before, after
}

func resultMatches(r searcher.Result) int64 {
	if r.Count > 0 {
		return int64(r.Count)
	}
	n := 0
	for _, lm := range r.Matches {
		if !lm.Context {
			n++
		}
	}
	return int64(n)
}

func allLowerPatterns(patterns []string) bool {
	for _, pattern := range patterns {
		for _, r := range pattern {
			if r >= 'A' && r <= 'Z' {
				return false
			}
		}
	}
	return true
}

func buildPathFilter() walker.Filter {
	includeFiles := []string(flagIncludes)
	excludeFiles := []string(flagExcludes)
	for _, glob := range flagGlobs {
		if strings.HasPrefix(glob, "!") {
			excludeFiles = append(excludeFiles, strings.TrimPrefix(glob, "!"))
		} else {
			includeFiles = append(includeFiles, glob)
		}
	}
	includeDirs := []string(flagIncludeDirs)
	excludeDirs := []string(flagExcludeDirs)
	includeTypeGlobs := buildTypeGlobs(flagTypes)
	excludeTypeGlobs := buildTypeGlobs(flagTypeExcludes)
	ignoreGlobs := buildIgnoreGlobs()
	cwd, _ := os.Getwd()

	return func(path string, d fs.DirEntry) bool {
		name := d.Name()
		if !*flagHidden && strings.HasPrefix(name, ".") {
			debugf("skip hidden: %s", path)
			return false
		}
		if !*flagNoDefaultIgnore && walker.IsDefaultIgnoredName(name) {
			debugf("skip default-ignore: %s", path)
			return false
		}
		if len(ignoreGlobs) > 0 && matchesAnyPathGlob(path, name, ignoreGlobs, cwd) {
			debugf("skip ignore: %s", path)
			return false
		}

		if d.IsDir() {
			return !matchesAnyPathGlob(path, name, excludeDirs, cwd) &&
				!matchesAnyPathGlob(path, name, excludeFiles, cwd)
		}

		if len(includeDirs) > 0 && !pathHasMatchingDir(path, includeDirs, cwd) {
			debugf("skip include-dir: %s", path)
			return false
		}
		if len(includeTypeGlobs) > 0 && !matchesAnyPathGlob(path, name, includeTypeGlobs, cwd) {
			debugf("skip type: %s", path)
			return false
		}
		if len(excludeTypeGlobs) > 0 && matchesAnyPathGlob(path, name, excludeTypeGlobs, cwd) {
			debugf("skip type-exclude: %s", path)
			return false
		}
		if matchesAnyPathGlob(path, name, excludeFiles, cwd) {
			debugf("skip exclude: %s", path)
			return false
		}
		if len(includeFiles) > 0 && !matchesAnyPathGlob(path, name, includeFiles, cwd) {
			debugf("skip include: %s", path)
			return false
		}
		return true
	}
}

func buildIgnoreGlobs() []string {
	if *flagNoIgnore {
		return nil
	}
	var globs []string
	for _, file := range []string{".gitignore", ".ignore", ".zrepignore"} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
				continue
			}
			line = filepath.ToSlash(line)
			line = strings.TrimPrefix(line, "/")
			if strings.HasSuffix(line, "/") {
				line = strings.TrimSuffix(line, "/")
			}
			globs = append(globs, line)
			if !strings.Contains(line, "/") {
				globs = append(globs, "**/"+line)
			}
		}
	}
	return globs
}

func buildTypeGlobs(selected []string) []string {
	if len(selected) == 0 {
		return nil
	}
	types := typeCatalog()
	var globs []string
	for _, typ := range selected {
		typ = strings.ToLower(typ)
		typeGlobs, ok := types[typ]
		if !ok {
			fatal(fmt.Errorf("unknown file type %q; run zrep -type-list", typ))
		}
		globs = append(globs, typeGlobs...)
	}
	return globs
}

func typeCatalog() map[string][]string {
	types := map[string][]string{
		"ada":         {"*.adb", "*.ads"},
		"asm":         {"*.asm", "*.s", "*.S", "*.nasm"},
		"audio":       {"*.aac", "*.aiff", "*.alac", "*.flac", "*.m4a", "*.mp3", "*.ogg", "*.opus", "*.wav", "*.wma"},
		"awk":         {"*.awk"},
		"bat":         {"*.bat", "*.cmd"},
		"binary":      {"*.bin", "*.dat", "*.db", "*.dll", "*.dylib", "*.exe", "*.o", "*.obj", "*.so"},
		"c":           {"*.c", "*.h"},
		"clojure":     {"*.clj", "*.cljs", "*.cljc", "*.edn"},
		"cmake":       {"CMakeLists.txt", "*.cmake"},
		"coffee":      {"*.coffee"},
		"config":      {"*.conf", "*.config", "*.cfg", "*.cnf", "*.ini", "*.properties", "*.prefs"},
		"cpp":         {"*.cc", "*.cpp", "*.cxx", "*.c++", "*.hpp", "*.hh", "*.hxx", "*.h++"},
		"csharp":      {"*.cs"},
		"css":         {"*.css", "*.scss", "*.sass", "*.less"},
		"csv":         {"*.csv", "*.tsv"},
		"dart":        {"*.dart"},
		"diff":        {"*.diff", "*.patch"},
		"docker":      {"Dockerfile", "Dockerfile.*", "*.dockerfile"},
		"doc":         {"*.doc", "*.docx", "*.odt", "*.pages", "*.pdf", "*.rtf"},
		"elixir":      {"*.ex", "*.exs"},
		"env":         {".env", ".env.*"},
		"erlang":      {"*.erl", "*.hrl"},
		"fortran":     {"*.f", "*.f77", "*.f90", "*.f95", "*.f03", "*.f08"},
		"go":          {"*.go"},
		"graphql":     {"*.graphql", "*.gql"},
		"groovy":      {"*.groovy", "*.gradle"},
		"haskell":     {"*.hs", "*.lhs"},
		"html":        {"*.html", "*.htm", "*.xhtml"},
		"image":       {"*.avif", "*.bmp", "*.gif", "*.heic", "*.ico", "*.jpeg", "*.jpg", "*.png", "*.tif", "*.tiff", "*.webp"},
		"java":        {"*.java", "*.jsp"},
		"js":          {"*.js", "*.jsx", "*.mjs", "*.cjs"},
		"json":        {"*.json", "*.jsonc", "*.jsonl", "*.ndjson"},
		"julia":       {"*.jl"},
		"kotlin":      {"*.kt", "*.kts"},
		"latex":       {"*.tex", "*.bib", "*.sty", "*.cls"},
		"lock":        {"*.lock", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "Cargo.lock", "go.sum"},
		"log":         {"*.log", "*.out", "*.err"},
		"lua":         {"*.lua"},
		"make":        {"Makefile", "makefile", "*.mk"},
		"man":         {"*.[1-9]"},
		"markdown":    {"*.md", "*.markdown", "*.mdown", "*.mkd"},
		"md":          {"*.md", "*.markdown", "*.mdown", "*.mkd"},
		"nim":         {"*.nim", "*.nims"},
		"nix":         {"*.nix"},
		"objc":        {"*.m", "*.mm"},
		"ocaml":       {"*.ml", "*.mli"},
		"perl":        {"*.pl", "*.pm", "*.pod", "*.t"},
		"php":         {"*.php", "*.phtml"},
		"powershell":  {"*.ps1", "*.psm1", "*.psd1"},
		"proto":       {"*.proto"},
		"python":      {"*.py", "*.pyw", "*.pyi"},
		"py":          {"*.py", "*.pyw", "*.pyi"},
		"r":           {"*.r", "*.R"},
		"ruby":        {"*.rb", "*.rake", "Gemfile", "Rakefile"},
		"rust":        {"*.rs"},
		"scala":       {"*.scala", "*.sc"},
		"sh":          {"*.sh", "*.bash", "*.zsh", "*.fish", "*.ksh"},
		"sql":         {"*.sql"},
		"spreadsheet": {"*.csv", "*.ods", "*.tsv", "*.xls", "*.xlsx"},
		"svelte":      {"*.svelte"},
		"swift":       {"*.swift"},
		"terraform":   {"*.tf", "*.tfvars"},
		"text":        {"*.txt", "*.text"},
		"toml":        {"*.toml"},
		"ts":          {"*.ts", "*.tsx"},
		"vb":          {"*.vb", "*.vbs"},
		"video":       {"*.avi", "*.m4v", "*.mkv", "*.mov", "*.mp4", "*.mpeg", "*.mpg", "*.webm", "*.wmv"},
		"vue":         {"*.vue"},
		"web":         {"*.astro", "*.css", "*.html", "*.htm", "*.js", "*.jsx", "*.less", "*.scss", "*.svelte", "*.ts", "*.tsx", "*.vue"},
		"windows":     {"*.bat", "*.cmd", "*.ini", "*.lnk", "*.ps1", "*.reg", "*.vbs"},
		"xml":         {"*.xml", "*.xsd", "*.xsl", "*.svg"},
		"yaml":        {"*.yaml", "*.yml"},
		"zig":         {"*.zig"},
		"zip":         {"*.7z", "*.bz2", "*.gz", "*.rar", "*.tar", "*.tgz", "*.xz", "*.zip", "*.zst"},
	}
	for _, add := range flagTypeAdds {
		name, glob, ok := strings.Cut(add, ":")
		if ok && name != "" && glob != "" {
			name = strings.ToLower(name)
			types[name] = append(types[name], glob)
		}
	}
	return types
}

func printTypeList() {
	types := typeCatalog()
	names := make([]string, 0, len(types))
	for name := range types {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Printf("%-12s %s\n", name, strings.Join(types[name], ", "))
	}
}

func pathHasMatchingDir(path string, patterns []string, cwd string) bool {
	dir := filepath.Dir(filepath.Clean(path))
	for {
		if dir == "." || dir == string(filepath.Separator) || dir == "" {
			return false
		}
		if matchesAnyPathGlob(dir, filepath.Base(dir), patterns, cwd) {
			return true
		}
		next := filepath.Dir(dir)
		if next == dir {
			return false
		}
		dir = next
	}
}

func matchesAnyPathGlob(path, name string, patterns []string, cwd string) bool {
	for _, pattern := range patterns {
		if matchesPathGlob(path, name, pattern, cwd) {
			return true
		}
	}
	return false
}

func matchesPathGlob(path, name, pattern, cwd string) bool {
	if pattern == "" {
		return false
	}
	if matchGlob(pattern, name) {
		return true
	}
	cleanPath := filepath.Clean(path)
	slashPath := filepath.ToSlash(cleanPath)
	slashPattern := filepath.ToSlash(pattern)
	if matchGlob(pattern, cleanPath) || matchGlob(slashPattern, slashPath) {
		return true
	}
	if !filepath.IsAbs(cleanPath) {
		return false
	}
	if cwd != "" {
		if rel, err := filepath.Rel(cwd, cleanPath); err == nil {
			slashRel := filepath.ToSlash(rel)
			return matchGlob(pattern, rel) ||
				matchGlob(slashPattern, slashRel) ||
				strings.Contains(slashPattern, "/") && strings.Contains(strings.ToLower(slashPath), strings.ToLower(slashPattern))
		}
	}
	return strings.Contains(slashPattern, "/") && strings.Contains(strings.ToLower(slashPath), strings.ToLower(slashPattern))
}

func matchGlob(pattern, value string) bool {
	if strings.HasPrefix(pattern, "**/") {
		tail := strings.TrimPrefix(pattern, "**/")
		if matchGlob(tail, filepath.Base(value)) || strings.HasSuffix(strings.ToLower(filepath.ToSlash(value)), "/"+strings.ToLower(tail)) {
			return true
		}
	}
	if ok, _ := filepath.Match(pattern, value); ok {
		return true
	}
	if ok, _ := filepath.Match(strings.ToLower(pattern), strings.ToLower(value)); ok {
		return true
	}
	slashPattern := strings.ReplaceAll(filepath.ToSlash(pattern), `\`, "/")
	slashValue := strings.ReplaceAll(filepath.ToSlash(value), `\`, "/")
	if ok, _ := path.Match(slashPattern, slashValue); ok {
		return true
	}
	ok, _ := path.Match(strings.ToLower(slashPattern), strings.ToLower(slashValue))
	return ok
}
