# zrep

`zrep` is an experimental high-performance grep/ripgrep-style search tool written in Go.

The current focus is a fast common core: fixed-string search, regex search, count mode, file listing, line numbers, only-matching output, invert match, repeated patterns, whole-word matching, and whole-line matching.

Large files are streamed line-by-line after the buffered/mmap threshold, so files in the multi-GB or TB range do not need to fit in memory. Matching output for large files is emitted in bounded chunks.

## Build

```sh
go build -o zrep .
```

## Usage

```sh
zrep [flags] PATTERN [PATH...]
zrep [flags] -e PATTERN [-e PATTERN...] [PATH...]
```

If no path is provided, `zrep` searches the current directory.

Normal match output is block-oriented. The location line uses
`path:line:start-column[-end-column]` so terminals and editors can Ctrl-click
the `path:line:column` portion, and the matching content is printed indented on
the next line.

```text
internal/matcher/matcher_test.go:36:48-51
    var corpus = generateCorpus(100_000, 80, 100, "TODO")
```

Examples:

```sh
zrep TODO .
zrep -F TODO internal
zrep -F -c TODO .
zrep -F -l TODO .
zrep -n 'func [A-Za-z]+' .
zrep -F -i -w error logs
zrep -F -o -e TODO -e FIXME .
zrep -F -v generated .
zrep -F -include '*.go' TODO .
zrep -F -include-dir internal -exclude-dir vendor TODO .
zrep -F -hidden -text NEEDLE dumps
```

## Supported Flags

```text
-F          fixed-string search
-e PATTERN  add a search pattern; may be repeated
-i          case-insensitive matching
-v          select non-matching lines
-w          match whole words
-x          match whole lines
-o          print only matching text
-l          print only filenames with matches
-c          print count of matching lines per file
-n          print line numbers
-H          always print filename prefixes
-h          never print filename prefixes
-no-color   disable ANSI color output
-j N        parallel workers; 0 = GOMAXPROCS*2
-cpuprofile FILE
-stats
-text       search binary files as text
-hidden     search hidden files and directories
-no-default-ignore
-include GLOB      include only files matching glob; repeatable
-exclude GLOB      exclude files or directories matching glob; repeatable
-include-dir GLOB  include only files under matching directories; repeatable
-exclude-dir GLOB  exclude directories matching glob; repeatable
```

## Benchmarks

The benchmark suite compares `zrep` against `rg` on a generated corpus.

```sh
go test -run '^$' -bench '^BenchmarkZrepVsRipgrep$' -benchtime=10x
```

The benchmark matrix currently covers:

```text
literal count
literal files-with-matches
literal printing
literal line numbers
case-insensitive literal count
regex count
only-matching output
invert count
whole-word count
whole-line count
multi-pattern count
include glob count
exclude glob count
include-dir count
exclude-dir count
hidden count
binary-as-text count
```

## Notes

`zrep` is not full ripgrep parity yet. Ripgrep supports a much larger surface area, including ignore-file semantics, type filters, encodings, multiline search, JSON output, PCRE2, replacements, and context modes.

This project is currently optimizing the core search pipeline first.
