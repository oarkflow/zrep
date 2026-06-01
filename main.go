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
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/oarkflow/bcl"
	xql "github.com/oarkflow/xql"
	"github.com/zrep/zrep/internal/matcher"
	"github.com/zrep/zrep/internal/output"
	"github.com/zrep/zrep/internal/query"
	"github.com/zrep/zrep/internal/records"
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
	flagProfile           = flag.String("profile", "", "apply a named config profile")
	flagPCRE2             = flag.Bool("pcre2", false, "use pure-Go advanced regex compatibility mode")
	flagFollow            = flag.Bool("follow", false, "follow symbolic links")
	flagMaxFileSize       = flag.String("max-filesize", "", "skip files larger than SIZE (K, M, G, T supported)")
	flagSearchCompressed  = flag.Bool("search-compressed", false, "search supported compressed files")
	flagFuzzy             = flag.Bool("fuzzy", false, "use approximate fuzzy matching")
	flagDistance          = flag.Int("distance", 2, "maximum fuzzy edit distance")
	flagBoolean           = flag.Bool("boolean", false, "parse pattern as a boolean expression")
	flagSemantic          = flag.Bool("semantic", false, "use local lexical semantic matching")
	flagLogs              = flag.Bool("logs", false, "parse lines as logs for time filtering and aggregation")
	flagActivity          = flag.String("activity", "", "filter log records by activity/action")
	flagUserID            = flag.String("user-id", "", "filter log records by user_id")
	flagEmail             = flag.String("email", "", "filter log records by email")
	flagIP                = flag.String("ip", "", "filter log records by ip address")
	flagSessionID         = flag.String("session-id", "", "filter log records by session_id")
	flagRequestID         = flag.String("request-id", "", "filter log records by request_id")
	flagQuery             = flag.String("query", "", "filter records with a boolean field expression")
	flagXQL               = flag.String("xql", "", "filter records with an XQL-style expression")
	flagSince             = flag.String("since", "", "only include logs since duration or time")
	flagFrom              = flag.String("from", "", "only include logs from time")
	flagTo                = flag.String("to", "", "only include logs until time")
	flagGroupBy           = flag.String("group-by", "", "group log or data results by field")
	flagHistogram         = flag.String("histogram", "", "print histogram by minute, hour, or day")
	flagWatch             = flag.Bool("watch", false, "watch files and rerun search")
	flagSchema            = flag.Bool("schema", false, "print discovered schema")
	flagProfileData       = flag.Bool("profile-data", false, "profile structured data")
	flagJQ                = flag.String("jq", "", "filter JSON/JSONL with a small FIELD==VALUE expression")
	flagSQL               = flag.String("sql", "", "query CSV/JSONL with a small SELECT ... WHERE ... expression")
	flagSQLFiles          = flag.Bool("sql-files", false, "search SQL files and print matching SQL blocks")
	flagSQLBlock          = flag.String("sql-block", "statement", "SQL block mode: statement, context, object, all")
	flagSQLContext        = flag.Int("sql-context", 2, "context lines for --sql-block context")
	flagSQLKind           = flag.String("sql-kind", "", "filter SQL blocks by kind")
	flagOutput            = flag.String("output", "", "alias for -format in inspect/data modes")
	flagTUI               = flag.Bool("tui", false, "open interactive terminal UI")
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
	flagIgnoreFiles       patternList
	flagFields            patternList
)

type zrepBCLConfig struct {
	Global   []string            `bcl:"global" json:"global"`
	Profiles map[string][]string `bcl:"profiles" json:"profiles"`
}

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
	flag.Var(&flagIgnoreFiles, "ignore-file", "read ignore globs from file; may be repeated")
	flag.Var(&flagFields, "field", "filter records by KEY=VALUE; may be repeated")
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
	if *flagOutput != "" {
		*flagFormat = *flagOutput
	}
	if *flagVersion {
		fmt.Println("zrep dev")
		return
	}
	if len(args) > 0 && runStateCommand(args) {
		return
	}
	if *flagTUI {
		fmt.Fprintln(os.Stderr, "zrep: --tui is reserved for the pure-Go interactive UI; use normal search flags for now")
		os.Exit(2)
	}
	if *flagWatch {
		fmt.Fprintln(os.Stderr, "zrep: --watch is reserved for live rerun/tail mode; use normal search flags for now")
		os.Exit(2)
	}
	if *flagTypeList {
		printTypeList()
		return
	}
	if !*flagLogs && !*flagSQLFiles && hasDataOperation() {
		if *flagSample == 0 && flagLookupChanged("limit") {
			*flagSample = *flagLimit
		}
		roots := args
		if len(roots) == 0 {
			roots = []string{"."}
		}
		if *flagSchema || *flagProfileData || *flagJQ != "" || *flagSQL != "" {
			runDataExplorer(roots)
			return
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
	if len(args) < 1 && len(flagPatterns) == 0 && !recordModeAllowsNoPattern() {
		flag.Usage()
		os.Exit(2)
	}

	patterns := []string(flagPatterns)
	roots := args
	if len(patterns) == 0 {
		if recordModeAllowsNoPattern() && hasRecordFilters() {
			if len(args) > 1 {
				patterns = []string{args[0]}
				roots = args[1:]
			} else {
				roots = args
			}
		} else {
			patterns = []string{args[0]}
			roots = args[1:]
		}
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
	if *flagLogs {
		runRecordLogs(patterns, roots)
		return
	}
	if *flagSQLFiles {
		runSQLFileSearch(patterns, roots)
		return
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
		if (*flagSearchArchives || *flagSearchCompressed) && searcher.IsSupportedArchive(entry.Path) {
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
	w.Follow = *flagFollow
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
	if isConfigManagementCommand(args) {
		return args, nil
	}
	if configDisabled(args) {
		return args, nil
	}
	profile := selectedConfigProfile(args)
	configPaths, err := configPathsForArgs(args, profile)
	if err != nil {
		return nil, err
	}
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
		tokens, err := readConfigArgs(path, profile)
		if err != nil {
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
	out = append(out, stripProfileArgs(args[1:])...)
	return out, nil
}

func isConfigManagementCommand(args []string) bool {
	valueFlags := flagsWithValues()
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if looksLikeFlag(arg) {
			if flagNeedsValue(arg, valueFlags) && i+1 < len(args) {
				i++
			}
			continue
		}
		switch arg {
		case "profile", "profiles", "config-profile":
			return true
		default:
			return false
		}
	}
	return false
}

func configPathsForArgs(args []string, profile string) ([]string, error) {
	if explicit := explicitConfigPaths(args); len(explicit) > 0 {
		return explicit, nil
	}
	if env := envConfigPaths(); len(env) > 0 {
		return env, nil
	}
	if profile == "" {
		return nil, nil
	}
	path, ok := firstExistingDefaultBCLConfigPath()
	if !ok {
		return nil, fmt.Errorf("config profile %q requested but no default config was found; create %s or pass --config", profile, strings.Join(defaultBCLConfigPaths(), " or "))
	}
	return []string{path}, nil
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

func envConfigPaths() []string {
	if path := os.Getenv("ZREP_CONFIG_PATH"); path != "" {
		return splitConfigPathList(path)
	}
	return nil
}

func defaultBCLConfigPaths() []string {
	paths := make([]string, 0, 2)
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "zrep", "config.bcl"))
	} else if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".config", "zrep", "config.bcl"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".zrep.bcl"))
	}
	return paths
}

func firstExistingDefaultBCLConfigPath() (string, bool) {
	for _, path := range defaultBCLConfigPaths() {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
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

func selectedConfigProfile(args []string) string {
	profile := strings.TrimSpace(os.Getenv("ZREP_PROFILE"))
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--profile" || arg == "-profile" || arg == "--config-id" || arg == "-config-id" {
			if i+1 < len(args) {
				profile = args[i+1]
				i++
			}
			continue
		}
		for _, prefix := range []string{"--profile=", "-profile=", "--config-id=", "-config-id="} {
			if value, ok := strings.CutPrefix(arg, prefix); ok {
				profile = value
				break
			}
		}
	}
	return strings.TrimSpace(profile)
}

func stripProfileArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--profile" || arg == "-profile" || arg == "--config-id" || arg == "-config-id" {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--profile=") || strings.HasPrefix(arg, "-profile=") ||
			strings.HasPrefix(arg, "--config-id=") || strings.HasPrefix(arg, "-config-id=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func readConfigArgs(path, profile string) ([]string, error) {
	if strings.EqualFold(filepath.Ext(path), ".bcl") {
		return readBCLConfigArgs(path, profile)
	}
	if profile != "" {
		debugf("profile %q ignored for flat config %s", profile, path)
	}
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

func readBCLConfigArgs(path, profile string) ([]string, error) {
	var cfg zrepBCLConfig
	if err := bcl.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	args := append([]string(nil), cfg.Global...)
	if profile == "" {
		return args, nil
	}
	profileArgs, ok := cfg.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("config profile %q not found in %s", profile, path)
	}
	args = append(args, profileArgs...)
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
	if (*flagSearchArchives || *flagSearchCompressed) && searcher.IsSupportedArchive(path) {
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
	w.Follow = *flagFollow
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
	flag.StringVar(flagProfile, "config-id", "", "alias for -profile")
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
	flag.BoolVar(flagPCRE2, "P", false, "alias for -pcre2")
	flag.BoolVar(flagFollow, "L", false, "alias for -follow")
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
		"A": true, "B": true, "C": true, "E": true, "N": true,
		"T": true, "e": true, "f": true, "g": true, "j": true, "m": true, "r": true, "t": true,
		"cell-width": true,
		"activity":   true, "config": true, "config-id": true, "cpuprofile": true, "distance": true, "email": true,
		"encoding": true, "exclude": true,
		"exclude-dir": true, "field": true, "file": true, "format": true, "glob": true, "include": true,
		"from": true, "group-by": true, "histogram": true, "ignore-file": true, "include-dir": true, "ip": true, "jq": true,
		"limit": true, "max-columns": true, "max-filesize": true,
		"max-cell-width": true, "max-count": true, "output": true, "path-separator": true, "profile": true, "replace": true,
		"query": true, "request-id": true, "sample": true, "select": true, "session-id": true, "since": true,
		"sort": true, "sortr": true, "sql": true, "sql-block": true, "sql-context": true, "sql-kind": true,
		"to": true, "type-add": true, "user-id": true, "where": true, "xql": true,
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
		'L': true, 'M': true, 'P': true, 'R': true, 'S': true, 'W': true, 'Y': true,
		'Z': true, 'c': true, 'h': true, 'i': true, 'j': true, 'l': true,
		'n': true, 'o': true, 'q': true, 'r': true, 's': true, 'v': true, 'w': true, 'x': true,
	}
}

func buildMatcher(patterns []string) (matcher.Matcher, bool, bool, error) {
	if *flagFuzzy || *flagSemantic {
		matchers := make([]matcher.Matcher, 0, len(patterns))
		for _, pattern := range patterns {
			matchers = append(matchers, matcher.NewFuzzy(pattern, *flagDistance))
		}
		return matcher.NewAny(matchers...), false, false, nil
	}
	if *flagBoolean {
		matchers := make([]matcher.Matcher, 0, len(patterns))
		for _, pattern := range patterns {
			m, err := matcher.NewBoolean(pattern, *flagIgnoreCase)
			if err != nil {
				return nil, false, false, err
			}
			matchers = append(matchers, m)
		}
		return matcher.NewAny(matchers...), false, false, nil
	}
	fixedFast := *flagFixed && !*flagWord && !*flagLine
	fixedWordFast := *flagFixed && *flagWord && !*flagLine
	if *flagPCRE2 {
		matchers := make([]matcher.Matcher, 0, len(patterns))
		for _, pattern := range patterns {
			m, err := matcher.NewAdvancedRegex(pattern, *flagIgnoreCase)
			if err != nil {
				return nil, false, false, err
			}
			matchers = append(matchers, m)
		}
		return matcher.NewAny(matchers...), false, false, nil
	}
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

func hasDataOperation() bool {
	return hasInspectOperation() || *flagSchema || *flagProfileData || *flagJQ != "" || *flagSQL != ""
}

func recordModeAllowsNoPattern() bool {
	return *flagLogs || *flagSQLFiles
}

func hasRecordFilters() bool {
	return *flagActivity != "" || *flagUserID != "" || *flagEmail != "" || *flagIP != "" ||
		*flagSessionID != "" || *flagRequestID != "" || *flagQuery != "" || *flagXQL != "" ||
		len(flagFields) > 0 || *flagSQLKind != "" || *flagGroupBy != "" || *flagHistogram != ""
}

func runStateCommand(args []string) bool {
	switch args[0] {
	case "profile", "profiles", "config-profile":
		if err := runProfileCommand(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
			os.Exit(2)
		}
		return true
	case "history":
		fmt.Println("zrep: query history is not recorded yet")
		return true
	case "save":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "zrep: save requires a name")
			os.Exit(2)
		}
		fmt.Printf("zrep: saved search %q support is reserved for the config store\n", args[1])
		return true
	case "run":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "zrep: run requires a saved search name")
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "zrep: saved search %q not found\n", args[1])
		os.Exit(2)
	}
	return false
}

func runProfileCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("profile command requires import, update, remove, or list")
	}
	cfgPath, err := writableBCLConfigPath()
	if err != nil {
		return err
	}
	cmd := args[0]
	switch cmd {
	case "list":
		cfg, err := readWritableBCLConfig(cfgPath)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(cfg.Profiles))
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Println(name)
		}
		return nil
	case "remove", "rm", "delete":
		if len(args) < 2 {
			return fmt.Errorf("profile remove requires an id")
		}
		cfg, err := readWritableBCLConfig(cfgPath)
		if err != nil {
			return err
		}
		id := strings.TrimSpace(args[1])
		if _, ok := cfg.Profiles[id]; !ok {
			return fmt.Errorf("config profile %q not found in %s", id, cfgPath)
		}
		delete(cfg.Profiles, id)
		if err := writeBCLConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("removed profile %q from %s\n", id, cfgPath)
		return nil
	case "import", "add":
		if len(args) < 3 {
			return fmt.Errorf("profile import requires an id and flags")
		}
		id, profileArgs := args[1], cleanProfileCommandArgs(args[2:])
		cfg, err := readWritableBCLConfig(cfgPath)
		if err != nil {
			return err
		}
		if _, exists := cfg.Profiles[id]; exists {
			return fmt.Errorf("config profile %q already exists; use profile update", id)
		}
		cfg.Profiles[id] = profileArgs
		if err := writeBCLConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("imported profile %q into %s\n", id, cfgPath)
		return nil
	case "update", "set":
		if len(args) < 3 {
			return fmt.Errorf("profile update requires an id and flags")
		}
		id, profileArgs := args[1], cleanProfileCommandArgs(args[2:])
		cfg, err := readWritableBCLConfig(cfgPath)
		if err != nil {
			return err
		}
		cfg.Profiles[id] = profileArgs
		if err := writeBCLConfig(cfgPath, cfg); err != nil {
			return err
		}
		fmt.Printf("updated profile %q in %s\n", id, cfgPath)
		return nil
	default:
		return fmt.Errorf("unknown profile command %q", cmd)
	}
}

func cleanProfileCommandArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func writableBCLConfigPath() (string, error) {
	if len(flagConfigFiles) > 0 {
		path := flagConfigFiles[len(flagConfigFiles)-1]
		if !strings.EqualFold(filepath.Ext(path), ".bcl") {
			return "", fmt.Errorf("profile commands require a .bcl config file")
		}
		return path, nil
	}
	if env := envConfigPaths(); len(env) > 0 {
		path := env[len(env)-1]
		if !strings.EqualFold(filepath.Ext(path), ".bcl") {
			return "", fmt.Errorf("profile commands require a .bcl config file")
		}
		return path, nil
	}
	if path, ok := firstExistingDefaultBCLConfigPath(); ok {
		return path, nil
	}
	paths := defaultBCLConfigPaths()
	if len(paths) == 0 {
		return "", fmt.Errorf("could not determine default config path")
	}
	return paths[0], nil
}

func readWritableBCLConfig(path string) (zrepBCLConfig, error) {
	cfg := zrepBCLConfig{Profiles: map[string][]string{}}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := bcl.DecodeFile(path, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string][]string{}
	}
	return cfg, nil
}

func writeBCLConfig(path string, cfg zrepBCLConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	writeBCLList(&b, "global", cfg.Global)
	b.WriteString("\nprofiles {\n")
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString("  ")
		b.WriteString(name)
		b.WriteByte(' ')
		writeInlineBCLList(&b, cfg.Profiles[name])
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeBCLList(b *strings.Builder, name string, values []string) {
	b.WriteString(name)
	b.WriteByte(' ')
	writeInlineBCLList(b, values)
	b.WriteByte('\n')
}

func writeInlineBCLList(b *strings.Builder, values []string) {
	b.WriteByte('[')
	for i, value := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(value))
	}
	b.WriteByte(']')
}

func runDataExplorer(roots []string) {
	workers := *flagWorkers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0) * 2
	}
	entries := collectEntries(roots, workers)
	sortEntries(entries, sortMode(), *flagSortReverse != "")
	for _, entry := range entries {
		if *flagSchema {
			printSchema(entry.Path)
		}
		if *flagProfileData {
			printDataProfile(entry.Path)
		}
		if *flagJQ != "" {
			printJSONPredicate(entry.Path, *flagJQ)
		}
		if *flagSQL != "" {
			printSQLQuery(entry.Path, *flagSQL)
		}
	}
}

func printSchema(file string) {
	b, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
		return
	}
	fmt.Printf("%s\n", formatOutputPath(file))
	ext := strings.ToLower(filepath.Ext(file))
	if ext == ".csv" || ext == ".tsv" {
		r := csv.NewReader(strings.NewReader(string(b)))
		if ext == ".tsv" {
			r.Comma = '\t'
		}
		header, err := r.Read()
		if err != nil {
			fmt.Println("  kind: delimited")
			return
		}
		for _, col := range header {
			fmt.Printf("  %s string\n", col)
		}
		return
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		fmt.Println("  kind: text")
		return
	}
	for _, line := range schemaLines("", v) {
		fmt.Println("  " + line)
	}
}

func schemaLines(prefix string, v any) []string {
	switch x := v.(type) {
	case []any:
		if len(x) == 0 {
			return []string{prefix + " array"}
		}
		return schemaLines(prefix+"[]", x[0])
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			name := k
			if prefix != "" {
				name = prefix + "." + k
			}
			out = append(out, schemaLines(name, x[k])...)
		}
		return out
	case string:
		return []string{prefix + " string"}
	case bool:
		return []string{prefix + " boolean"}
	case float64:
		return []string{prefix + " number"}
	case nil:
		return []string{prefix + " null"}
	default:
		return []string{prefix + " value"}
	}
}

func printDataProfile(file string) {
	f, err := os.Open(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
		return
	}
	defer f.Close()
	r := csv.NewReader(f)
	if strings.EqualFold(filepath.Ext(file), ".tsv") {
		r.Comma = '\t'
	}
	header, err := r.Read()
	if err != nil {
		fmt.Printf("%s\n  rows: 0\n", formatOutputPath(file))
		return
	}
	uniques := make([]map[string]int, len(header))
	nulls := make([]int, len(header))
	for i := range uniques {
		uniques[i] = map[string]int{}
	}
	rows := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		rows++
		for i := range header {
			val := ""
			if i < len(rec) {
				val = rec[i]
			}
			if val == "" {
				nulls[i]++
			}
			uniques[i][val]++
		}
	}
	fmt.Printf("%s\n  rows: %d\n", formatOutputPath(file), rows)
	for i, col := range header {
		dupes := 0
		for _, n := range uniques[i] {
			if n > 1 {
				dupes += n - 1
			}
		}
		fmt.Printf("  %s nulls=%d uniques=%d duplicates=%d\n", col, nulls[i], len(uniques[i]), dupes)
	}
}

func printJSONPredicate(file, expr string) {
	field, value, ok := parseFieldPredicate(expr)
	if !ok {
		fmt.Fprintf(os.Stderr, "zrep: unsupported --jq expression %q\n", expr)
		return
	}
	for _, obj := range readJSONObjects(file) {
		if fmt.Sprint(fieldValue(obj, field)) == value {
			b, _ := json.Marshal(obj)
			fmt.Println(string(b))
		}
	}
}

func printSQLQuery(file, query string) {
	fields, whereField, whereValue := parseMiniSQL(query)
	for _, obj := range readDelimitedObjects(file) {
		if whereField != "" && fmt.Sprint(obj[whereField]) != whereValue {
			continue
		}
		if len(fields) == 0 || fields[0] == "*" {
			b, _ := json.Marshal(obj)
			fmt.Println(string(b))
			continue
		}
		row := make([]string, len(fields))
		for i, f := range fields {
			row[i] = fmt.Sprint(obj[f])
		}
		fmt.Println(strings.Join(row, ","))
	}
}

func parseFieldPredicate(expr string) (string, string, bool) {
	for _, op := range []string{"==", "="} {
		if left, right, ok := strings.Cut(expr, op); ok {
			return strings.Trim(strings.TrimSpace(left), "."), strings.Trim(strings.Trim(strings.TrimSpace(right), `"`), `'`), true
		}
	}
	return "", "", false
}

func parseMiniSQL(query string) ([]string, string, string) {
	upper := strings.ToUpper(query)
	if !strings.HasPrefix(upper, "SELECT ") {
		return nil, "", ""
	}
	body := strings.TrimSpace(query[len("SELECT "):])
	selectPart := body
	wherePart := ""
	if idx := strings.Index(strings.ToUpper(body), " WHERE "); idx >= 0 {
		selectPart = strings.TrimSpace(body[:idx])
		wherePart = strings.TrimSpace(body[idx+7:])
	}
	fields := strings.Split(selectPart, ",")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	field, value, _ := parseFieldPredicate(wherePart)
	return fields, field, value
}

func readJSONObjects(file string) []map[string]any {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var arr []map[string]any
	if json.Unmarshal(b, &arr) == nil {
		return arr
	}
	var obj map[string]any
	if json.Unmarshal(b, &obj) == nil {
		return []map[string]any{obj}
	}
	var out []map[string]any
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		obj = map[string]any{}
		if json.Unmarshal([]byte(line), &obj) == nil {
			out = append(out, obj)
		}
	}
	return out
}

func readDelimitedObjects(file string) []map[string]string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	r := csv.NewReader(f)
	if strings.EqualFold(filepath.Ext(file), ".tsv") {
		r.Comma = '\t'
	}
	header, err := r.Read()
	if err != nil {
		return nil
	}
	var out []map[string]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		obj := map[string]string{}
		for i, h := range header {
			if i < len(rec) {
				obj[h] = rec[i]
			}
		}
		out = append(out, obj)
	}
	return out
}

func fieldValue(obj map[string]any, field string) any {
	var cur any = obj
	for _, part := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func runRecordLogs(patterns, roots []string) {
	pred, err := buildRecordPredicate()
	if err != nil {
		fatal(err)
	}
	var out []records.Record
	for _, entry := range collectEntries(roots, runtime.GOMAXPROCS(0)*2) {
		recs, err := records.ReadLogRecords(entry.Path)
		if err != nil {
			if !*flagNoMessages {
				fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
			}
			continue
		}
		for _, rec := range recs {
			if !recordTimeInRange(rec.Fields["timestamp"]) || !recordMatchesPattern(rec, patterns) || !pred.Match(rec.Fields) {
				continue
			}
			out = append(out, rec)
		}
	}
	out, err = applyXQLFilter(out)
	if err != nil {
		fatal(err)
	}
	if *flagGroupBy != "" || *flagHistogram != "" {
		printRecordSummary(out)
		return
	}
	printRecords(out)
}

func runSQLFileSearch(patterns, roots []string) {
	pred, err := buildRecordPredicate()
	if err != nil {
		fatal(err)
	}
	mode := strings.ToLower(strings.TrimSpace(*flagSQLBlock))
	if mode == "" {
		mode = "statement"
	}
	var out []records.Record
	for _, entry := range collectEntries(roots, runtime.GOMAXPROCS(0)*2) {
		if !strings.EqualFold(filepath.Ext(entry.Path), ".sql") {
			continue
		}
		blocks, err := records.ReadSQLBlocks(entry.Path, mode, *flagSQLContext)
		if err != nil {
			if !*flagNoMessages {
				fmt.Fprintf(os.Stderr, "zrep: %v\n", err)
			}
			continue
		}
		for _, block := range blocks {
			if *flagSQLKind != "" && !strings.EqualFold(block.Kind, *flagSQLKind) {
				continue
			}
			rec := block.Record
			if !recordMatchesPattern(rec, patterns) || !pred.Match(rec.Fields) {
				continue
			}
			out = append(out, rec)
		}
	}
	out, err = applyXQLFilter(out)
	if err != nil {
		fatal(err)
	}
	printRecords(out)
}

func buildRecordPredicate() (query.Predicate, error) {
	var preds []query.Predicate
	add := func(field, value string) {
		if strings.TrimSpace(value) != "" {
			preds = append(preds, query.FieldEquals(field, value))
		}
	}
	add("activity", *flagActivity)
	add("user_id", *flagUserID)
	add("email", *flagEmail)
	add("ip", *flagIP)
	add("session_id", *flagSessionID)
	add("request_id", *flagRequestID)
	for _, field := range flagFields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return nil, fmt.Errorf("--field requires KEY=VALUE, got %q", field)
		}
		add(key, value)
	}
	if strings.TrimSpace(*flagQuery) != "" {
		pred, err := query.Parse(*flagQuery)
		if err != nil {
			return nil, err
		}
		preds = append(preds, pred)
	}
	return query.And(preds...), nil
}

func applyXQLFilter(recs []records.Record) ([]records.Record, error) {
	expr := strings.TrimSpace(*flagXQL)
	if expr == "" || len(recs) == 0 {
		return recs, nil
	}
	queryText := xqlRecordQuery(expr)
	xqlRows, maps := xqlRowsForRecords(recs)
	cat := xql.NewInMemoryCatalog()
	cat.RegisterWithRows("zrep_records", xql.InferSchemaFromRows("zrep_records", maps), xqlRows)
	result, _, err := xql.RunResult(context.Background(), queryText, cat)
	if err != nil {
		return nil, fmt.Errorf("--xql: %w", err)
	}
	keep := map[int]bool{}
	for _, row := range result.Rows {
		idx, ok := xqlRowIndex(row["__zrep_index"])
		if !ok {
			return nil, fmt.Errorf("--xql query must preserve __zrep_index; use zrep_records | where ... or select __zrep_index")
		}
		if idx >= 0 && idx < len(recs) {
			keep[idx] = true
		}
	}
	out := make([]records.Record, 0, len(keep))
	for i, rec := range recs {
		if keep[i] {
			out = append(out, rec)
		}
	}
	return out, nil
}

func xqlRecordQuery(expr string) string {
	lower := strings.ToLower(strings.TrimSpace(expr))
	if strings.HasPrefix(lower, "where ") {
		return "zrep_records | " + expr + " | select __zrep_index"
	}
	if strings.Contains(expr, "|") || strings.HasPrefix(lower, "zrep_records") || strings.HasPrefix(lower, "from ") {
		return expr
	}
	return "zrep_records | where " + expr + " | select __zrep_index"
}

func xqlRowsForRecords(recs []records.Record) ([]xql.Row, []map[string]any) {
	rows := make([]xql.Row, 0, len(recs))
	maps := make([]map[string]any, 0, len(recs))
	for i, rec := range recs {
		row := xql.Row{
			"__zrep_index": i,
			"path":         rec.Path,
			"line_start":   rec.LineStart,
			"line_end":     rec.LineEnd,
			"column":       rec.Column,
			"text":         rec.Text,
		}
		for key, value := range rec.Fields {
			row[key] = value
		}
		rows = append(rows, row)
		m := make(map[string]any, len(row))
		for key, value := range row {
			m[key] = value
		}
		maps = append(maps, m)
	}
	return rows, maps
}

func xqlRowIndex(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	default:
		return 0, false
	}
}

func recordMatchesPattern(rec records.Record, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	haystack := rec.Text
	for key, value := range rec.Fields {
		haystack += "\n" + key + "=" + value
	}
	if *flagIgnoreCase || *flagSmartCase && allLowerPatterns(patterns) {
		haystack = strings.ToLower(haystack)
	}
	for _, pattern := range patterns {
		p := pattern
		if *flagIgnoreCase || *flagSmartCase && allLowerPatterns(patterns) {
			p = strings.ToLower(p)
		}
		if strings.Contains(haystack, p) {
			return true
		}
	}
	return false
}

func recordTimeInRange(value string) bool {
	if value == "" {
		return true
	}
	ts, ok := parseUserTime(value)
	if !ok {
		return true
	}
	if *flagSince != "" {
		if d, err := time.ParseDuration(*flagSince); err == nil && ts.Before(time.Now().Add(-d)) {
			return false
		}
	}
	if *flagFrom != "" {
		if t, ok := parseUserTime(*flagFrom); ok && ts.Before(t) {
			return false
		}
	}
	if *flagTo != "" {
		if t, ok := parseUserTime(*flagTo); ok && ts.After(t) {
			return false
		}
	}
	return true
}

func printRecordSummary(recs []records.Record) {
	counts := map[string]int{}
	for _, rec := range recs {
		key := "matches"
		if *flagHistogram != "" {
			key = histogramBucket(rec.Fields["timestamp"], *flagHistogram)
		} else if *flagGroupBy != "" {
			key = rec.Fields[strings.ToLower(*flagGroupBy)]
		}
		if key == "" {
			key = "(missing)"
		}
		counts[key]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Printf("%s %d\n", key, counts[key])
	}
}

func printRecords(recs []records.Record) {
	switch strings.ToLower(*flagFormat) {
	case "json":
		for _, rec := range recs {
			_ = json.NewEncoder(os.Stdout).Encode(recordOutput(rec))
		}
	case "pretty-json":
		b, _ := json.MarshalIndent(recordOutputs(recs), "", "  ")
		fmt.Println(string(b))
	case "table":
		printRecordTable(recs)
	case "csv":
		printRecordCSV(recs)
	default:
		for _, rec := range recs {
			printRecordPlain(rec)
		}
	}
}

func recordOutputs(recs []records.Record) []map[string]any {
	out := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		out = append(out, recordOutput(rec))
	}
	return out
}

func recordOutput(rec records.Record) map[string]any {
	row := map[string]any{
		"path":       formatOutputPath(rec.Path),
		"line_start": rec.LineStart,
		"line_end":   rec.LineEnd,
		"text":       rec.Text,
	}
	for k, v := range rec.Fields {
		row[k] = v
	}
	return row
}

func printRecordPlain(rec records.Record) {
	path := formatOutputPath(rec.Path)
	if rec.LineEnd > rec.LineStart {
		fmt.Printf("%s:%d-%d\n", path, rec.LineStart, rec.LineEnd)
	} else {
		fmt.Printf("%s:%d:%d\n", path, rec.LineStart, max(1, rec.Column))
	}
	for _, line := range strings.Split(rec.Text, "\n") {
		fmt.Println("    " + line)
	}
}

func printRecordTable(recs []records.Record) {
	fields := selectedRecordFields(recs)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(fields, "\t"))
	for _, rec := range recs {
		values := make([]string, len(fields))
		row := recordOutput(rec)
		for i, field := range fields {
			values[i] = tableCell(fmt.Sprint(row[field]))
		}
		fmt.Fprintln(tw, strings.Join(values, "\t"))
	}
	tw.Flush()
}

func printRecordCSV(recs []records.Record) {
	fields := selectedRecordFields(recs)
	w := csv.NewWriter(os.Stdout)
	_ = w.Write(fields)
	for _, rec := range recs {
		row := recordOutput(rec)
		values := make([]string, len(fields))
		for i, field := range fields {
			values[i] = fmt.Sprint(row[field])
		}
		_ = w.Write(values)
	}
	w.Flush()
}

func selectedRecordFields(recs []records.Record) []string {
	if selected := selectedInspectFields(); len(selected) > 0 {
		return selected
	}
	fields := []string{"path", "line_start", "line_end", "timestamp", "level", "user_id", "email", "activity", "service", "ip", "request_id", "session_id", "kind", "table", "object", "text"}
	seen := map[string]bool{}
	var out []string
	for _, field := range fields {
		for _, rec := range recs {
			if field == "path" || field == "line_start" || field == "line_end" || rec.Fields[field] != "" {
				if !seen[field] {
					seen[field] = true
					out = append(out, field)
				}
				break
			}
		}
	}
	if len(out) == 0 {
		return []string{"path", "line_start", "text"}
	}
	return out
}

func histogramBucket(value, unit string) string {
	ts, ok := parseUserTime(value)
	if !ok {
		return "unknown"
	}
	switch strings.ToLower(unit) {
	case "minute":
		return ts.Format("2006-01-02 15:04")
	case "day":
		return ts.Format("2006-01-02")
	default:
		return ts.Format("2006-01-02 15:00")
	}
}

func parseUserTime(value string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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
	maxFileSize := int64(0)
	if *flagMaxFileSize != "" {
		var err error
		maxFileSize, err = parseSize(*flagMaxFileSize)
		if err != nil {
			fatal(err)
		}
	}
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
		if maxFileSize > 0 {
			if info, err := d.Info(); err == nil && info.Size() > maxFileSize {
				debugf("skip max-filesize: %s", path)
				return false
			}
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
	for _, file := range append([]string{".gitignore", ".ignore", ".rgignore", ".zrepignore"}, []string(flagIgnoreFiles)...) {
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

func parseSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	mult := int64(1)
	last := value[len(value)-1]
	switch last {
	case 'k', 'K':
		mult = 1 << 10
		value = value[:len(value)-1]
	case 'm', 'M':
		mult = 1 << 20
		value = value[:len(value)-1]
	case 'g', 'G':
		mult = 1 << 30
		value = value[:len(value)-1]
	case 't', 'T':
		mult = 1 << 40
		value = value[:len(value)-1]
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --max-filesize %q", value)
	}
	return int64(n * float64(mult)), nil
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
		"config":      {"*.config", "*.cfg", "*.cnf", "*.ini", "*.properties", "*.prefs"},
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
