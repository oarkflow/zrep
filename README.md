# zrep

`zrep` is an experimental high-performance grep/ripgrep-style search tool written in Go.

The current focus is a fast common core: fixed-string search, regex search, count mode, file listing, line numbers, only-matching output, invert match, repeated patterns, whole-word matching, and whole-line matching.

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
```

## Notes

`zrep` is not full ripgrep parity yet. Ripgrep supports a much larger surface area, including ignore-file semantics, globs, type filters, encodings, multiline search, JSON output, PCRE2, replacements, and context modes.

This project is currently optimizing the core search pipeline first.

