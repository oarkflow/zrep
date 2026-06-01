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

When searching a single file, zrep omits the filename and prints navigable
line/column locations:

```text
36:48-51
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
zrep -F -f patterns.txt .
zrep -F -v generated .
zrep --files .
zrep --files-without-match TODO .
zrep -F --count-matches TODO .
zrep -F --max-count 3 TODO .
zrep -F --passthru TODO file.txt
zrep -F --json-events TODO .
zrep -F --glob '*.go' TODO .
zrep -F --glob '!*.gen.go' TODO .
zrep -F --sort path TODO .
zrep --config ~/.config/zrep/work.conf TODO .
ZREP_CONFIG_PATH=~/.config/zrep/work.conf zrep TODO .
zrep -F -include '*.go' TODO .
zrep -F -include-dir internal -exclude-dir vendor TODO .
zrep -F -hidden -text NEEDLE dumps
zrep -S todo .
zrep -F -C 2 TODO internal
zrep -F -json TODO .
zrep -F -vimgrep TODO .
zrep -F -replace DONE TODO .
zrep -F -replace DONE -write TODO file.txt
zrep -multiline 'TODO(.|\n)*DONE' logs
zrep -search-archives TODO artifacts
zrep -encoding utf-16le TODO windows.txt
zrep -F -t go TODO .
zrep -F -t csv -t json customer_id data
zrep -F -t js -t ts -t web TODO app
zrep -F -T json TODO .
zrep -type-list
zrep -inspect -sample 5 data.csv
zrep -rows -columns -sample 3 data.json
zrep -json -inspect -sample 2 data/
zrep -inspect -sample 2 -format pretty-json data.json
zrep -inspect -sample 10 -format table data.csv
zrep -inspect --select id,login --format table data.json
zrep -inspect --select id,user.name --flatten --where login=b --format table data.json
zrep -JIRK data.json
zrep -jisrc data.csv
```

## Large JSON Examples

Examples for a large JSON file at `~/Downloads/large-file.json`:

```sh
# Detect kind, count records, show keys, and sample the first 5 records.
zrep -inspect -sample 5 ~/Downloads/large-file.json

# Only print total JSON records/rows.
zrep -rows ~/Downloads/large-file.json

# Print object keys or keys from the first array element.
zrep -columns ~/Downloads/large-file.json

# Machine-readable inspection output.
zrep -json -inspect -sample 3 ~/Downloads/large-file.json

# Indented structured JSON output with sampled records as nested objects.
zrep -inspect -sample 3 -format pretty-json ~/Downloads/large-file.json

# Table output for quick terminal inspection.
zrep -inspect -sample 10 -format table ~/Downloads/large-file.json
zrep -json -inspect -sample 3 ~/Downloads/large-file.json --format table
zrep -json -inspect -sample 3 ~/Downloads/large-file.json --format table --max-cell-width 40
zrep -inspect --select id,login --limit 10 --format table ~/Downloads/large-file.json
zrep -inspect --select id,user.name --flatten --where login=petroav --format table ~/Downloads/large-file.json

# Disable table truncation when you need the full cell text.
zrep -inspect -sample 3 -format table -max-cell-width 0 ~/Downloads/large-file.json

# CSV-formatted summary output.
zrep -inspect -sample 3 -format csv ~/Downloads/large-file.json

# Search for a customer id/value with navigable line and column output.
zrep -F 'customer_id' ~/Downloads/large-file.json

# Search with context around matching lines.
zrep -F -C 2 'customer_id' ~/Downloads/large-file.json

# Count matching lines only.
zrep -F -c 'customer_id' ~/Downloads/large-file.json

# Show search performance stats.
zrep -F -stats 'customer_id' ~/Downloads/large-file.json

# Use compact short aliases: JSON + inspect + rows + columns.
zrep -JIRK ~/Downloads/large-file.json
```

## Supported Flags

```text
-F          fixed-string search
-e PATTERN  add a search pattern; may be repeated
-f FILE     add patterns from a file; may be repeated
-i          case-insensitive matching
-S          smart-case matching
-v          select non-matching lines
-w          match whole words
-x          match whole lines
-o          print only matching text
-l          print only filenames with matches
-files-without-match
-files      print searchable files without searching
-c          print count of matching lines per file
-count-matches
-include-zero
-m N        stop after N matching lines per file
-q          quiet; exit after the first match
-n          print line numbers
-H          always print filename prefixes
-h          never print filename prefixes
-no-color   disable ANSI color output
-j N        parallel workers; 0 = GOMAXPROCS*2
-cpuprofile FILE
-stats      print files, bytes, matches, and elapsed time to stderr
-A N        print N lines after each match
-B N        print N lines before each match
-C N        print N lines before and after each match
-json       print newline-delimited JSON results
-json-events print begin/match/context/end/summary JSON records
-vimgrep    print file:line:column:text results
-heading    group normal block output under file headings
-pretty     print pretty block output (default)
-replace TEXT
-write      apply --replace changes to regular files
-multiline  allow matches to span multiple lines
-search-archives
-encoding NAME     auto, utf-8, utf-16le, utf-16be
-passthru   print every line, highlighting matches
-trim       trim leading whitespace in printed lines
-max-columns N
-max-columns-preview
-0          terminate path records with NUL
-path-separator SEP
-no-messages
-debug
-version
-config FILE read flags from a config file
-no-config  do not read config files
-text       search binary files as text
-hidden     search hidden files and directories
-no-ignore  do not read .gitignore, .ignore, or .zrepignore
-no-default-ignore
-include GLOB      include only files matching glob; repeatable
-exclude GLOB      exclude files or directories matching glob; repeatable
-g GLOB            include glob, or exclude with !GLOB; repeatable
-include-dir GLOB  include only files under matching directories; repeatable
-exclude-dir GLOB  exclude directories matching glob; repeatable
-t TYPE           include files of known type; repeatable
-T TYPE           exclude files of known type; repeatable
-type-add NAME:GLOB
-type-list        list supported file types
-inspect          auto-detect kind and print rows, columns, and samples
-rows             print total rows/records per file
-columns          print CSV columns or JSON object keys
-sample N         print first N lines/records per file
-limit N          alias for -sample in inspect mode
-format FORMAT    inspect format: plain, json, pretty-json, table, csv
-max-cell-width N maximum table cell width; 0 disables truncation
-select FIELDS    inspect CSV columns or JSON fields
-flatten          flatten nested JSON into dot-path fields
-where FIELD=VAL  inspect only matching JSON/CSV sample rows
-sort FIELD       sort by path, modified, or size
-sortr FIELD      reverse sort by path, modified, or size
```

Short boolean flags can be clustered, so `-ivc` is treated as `-i -v -c`.
Uppercase short aliases are used for newer modes to avoid collisions:

```text
-J  alias for -json
-G  alias for -vimgrep
-P  alias for -pretty
-I  alias for -inspect
-R  alias for -rows
-K  alias for -columns
-M  alias for -multiline
-Z  alias for -search-archives
-W  alias for -write
-Y  alias for -type-list
-s  alias for -stats
-r TEXT  alias for -replace TEXT
-E NAME  alias for -encoding NAME
-N NUM   alias for -sample NUM
-L NUM   alias for -limit NUM
```

Long zrep-friendly aliases include `--list-files`, `--without-match`,
`--matches-count`, `--all-lines`, and `--cell-width`.

Inside a clustered flag only, lowercase `j` maps to `-J` and lowercase `r`
maps to `-R`, so compact forms like `-jisrc` work while standalone `-j N`
still means worker count and standalone `-r TEXT` still means replace.

## Flag Examples And Responses

The examples below use `<ROOT>` as a placeholder for the searched directory.
Tests verify that every registered flag has a documented example marker here.

<!-- flag:F -->
### `-F`
Purpose: treat the pattern as a fixed string.
```sh
zrep -F TODO <ROOT>/a.txt
```
```text
1:7-10
    alpha TODO one
3:7-10
    alpha TODO TODO three
```

<!-- flag:e -->
### `-e PATTERN`
Purpose: add one or more patterns.
```sh
zrep -F -e TODO -e FIXME <ROOT>/a.txt
```
```text
1:7-10
    alpha TODO one
4:1-5
    FIXME item
```

<!-- flag:f --><!-- flag:file -->
### `-f FILE`, `--file FILE`
Purpose: read one pattern per line from a pattern file.
```sh
zrep -F -f <ROOT>/patterns.txt -c <ROOT> --include-zero --sort path
```
```text
<ROOT>/a.txt:3
<ROOT>/b.log:0
```

<!-- flag:i -->
### `-i`
Purpose: case-insensitive search.
```sh
zrep -F -i todo <ROOT>/a.txt
```
```text
1:7-10
    alpha TODO one
```

<!-- flag:S -->
### `-S`
Purpose: smart case; lowercase patterns search case-insensitively.
```sh
zrep -F -S todo <ROOT>/a.txt
```
```text
1:7-10
    alpha TODO one
```

<!-- flag:v -->
### `-v`
Purpose: print non-matching lines.
```sh
zrep -F -v TODO <ROOT>/a.txt
```
```text
2:1
    beta two
4:1
    FIXME item
```

<!-- flag:w -->
### `-w`
Purpose: match whole words.
```sh
zrep -F -w TODO <ROOT>/words.txt
```
```text
1:1-4
    TODO todoish
```

<!-- flag:x -->
### `-x`
Purpose: match whole lines.
```sh
zrep -F -x TODO <ROOT>/lines.txt
```
```text
1:1-4
    TODO
```

<!-- flag:o -->
### `-o`
Purpose: print only matching text.
```sh
zrep -F -o TODO <ROOT>/a.txt
```
```text
TODO
TODO
TODO
```

<!-- flag:l -->
### `-l`
Purpose: print files with matches.
```sh
zrep -F -l TODO <ROOT> --sort path
```
```text
<ROOT>/a.txt
<ROOT>/lines.txt
```

<!-- flag:files --><!-- flag:list-files -->
### `--files`, `--list-files`
Purpose: print files that would be searched without searching.
```sh
zrep --files <ROOT> --sort path
```
```text
<ROOT>/a.txt
<ROOT>/data.json
```

<!-- flag:files-without-match --><!-- flag:without-match -->
### `--files-without-match`, `--without-match`
Purpose: print files with zero matches.
```sh
zrep -F --files-without-match TODO <ROOT> --sort path
```
```text
<ROOT>/b.log
<ROOT>/data.csv
```

<!-- flag:c -->
### `-c`
Purpose: count matching lines.
```sh
zrep -F -c TODO <ROOT>/a.txt
```
```text
2
```

<!-- flag:count-matches --><!-- flag:matches-count -->
### `--count-matches`, `--matches-count`
Purpose: count individual matches.
```sh
zrep -F --count-matches TODO <ROOT>/a.txt
```
```text
3
```

<!-- flag:include-zero -->
### `--include-zero`
Purpose: include files with zero count in count mode.
```sh
zrep -F -c TODO <ROOT> --include-zero --sort path
```
```text
<ROOT>/a.txt:2
<ROOT>/b.log:0
```

<!-- flag:m --><!-- flag:max-count -->
### `-m N`, `--max-count N`
Purpose: stop after N matching lines per file.
```sh
zrep -F --max-count 1 TODO <ROOT>/a.txt
```
```text
1:7-10
    alpha TODO one
```

<!-- flag:q --><!-- flag:quiet -->
### `-q`, `--quiet`
Purpose: suppress output and use exit status.
```sh
zrep -F -q TODO <ROOT>/a.txt
```
```text
stdout: empty
exit: 0
```

<!-- flag:n -->
### `-n`
Purpose: request line-numbered output.
```sh
zrep -F -n TODO <ROOT>/a.txt
```
```text
1:7-10
    alpha TODO one
```

<!-- flag:H -->
### `-H`
Purpose: always include filename.
```sh
zrep -F -H TODO <ROOT>/a.txt
```
```text
<ROOT>/a.txt:1:7-10
    alpha TODO one
```

<!-- flag:h -->
### `-h`
Purpose: suppress filename.
```sh
zrep -F -h TODO <ROOT> --sort path
```
```text
1:7-10
    alpha TODO one
```

<!-- flag:no-color -->
### `--no-color`
Purpose: disable ANSI color.
```sh
zrep -F --no-color TODO <ROOT>/a.txt
```
```text
1:7-10
    alpha TODO one
```

<!-- flag:j -->
### `-j N`
Purpose: set worker count.
```sh
zrep -F -j 1 -c TODO <ROOT>/a.txt
```
```text
2
```

<!-- flag:A --><!-- flag:B --><!-- flag:C -->
### `-A N`, `-B N`, `-C N`
Purpose: show after, before, or combined context lines.
```sh
zrep -F -C 1 "TODO TODO" <ROOT>/a.txt
```
```text
2:1
    beta two
3:7-15
    alpha TODO TODO three
4:1
    FIXME item
```

<!-- flag:json --><!-- flag:J -->
### `--json`, `-J`
Purpose: print newline-delimited JSON match records.
```sh
zrep -F -J "TODO one" <ROOT>/a.txt
```
```json
{"path":"<ROOT>/a.txt","line":1,"column":7,"end_column":14,"context":false,"text":"alpha TODO one"}
```

<!-- flag:json-events -->
### `--json-events`
Purpose: print begin, match, end, and summary JSON events.
```sh
zrep -F --json-events "TODO one" <ROOT>/a.txt
```
```json
{"type":"begin","data":{"path":{"text":"<ROOT>/a.txt"}}}
{"type":"match","data":{"line":1,"column":7,"text":{"text":"alpha TODO one"}}}
{"type":"end","data":{"matches":1}}
{"type":"summary","data":{"files":1,"matches":1}}
```

<!-- flag:vimgrep --><!-- flag:G -->
### `--vimgrep`, `-G`
Purpose: print editor-friendly `file:line:column:text`.
```sh
zrep -F -G "TODO one" <ROOT>/a.txt
```
```text
<ROOT>/a.txt:1:7:alpha TODO one
```

<!-- flag:heading -->
### `--heading`
Purpose: group matches under file headings.
```sh
zrep -F --heading TODO <ROOT>/a.txt
```
```text
<ROOT>/a.txt
1:7-10
    alpha TODO one
```

<!-- flag:pretty --><!-- flag:P -->
### `--pretty`, `-P`
Purpose: force pretty block output.
```sh
zrep -F -P "TODO one" <ROOT>/a.txt
```
```text
1:7-14
    alpha TODO one
```

<!-- flag:replace --><!-- flag:r -->
### `--replace TEXT`, `-r TEXT`
Purpose: preview replacement text in output.
```sh
zrep -F -r DONE TODO <ROOT>/a.txt
```
```text
1:7-10
    alpha DONE one
```

<!-- flag:write --><!-- flag:W -->
### `--write`, `-W`
Purpose: apply replacement to files.
```sh
zrep -F --replace DONE --write TODO <ROOT>/write.txt
```
```text
1
```

<!-- flag:multiline --><!-- flag:M -->
### `--multiline`, `-M`
Purpose: allow matches across line breaks.
```sh
zrep -M 'start(.|\n)*end' <ROOT>/multi.txt
```
```text
1:1-22
    start
middle TODO
end
```

<!-- flag:search-archives --><!-- flag:Z -->
### `--search-archives`, `-Z`
Purpose: search supported archives.
```sh
zrep -F -Z ZIPTODO <ROOT>/archive.zip
```
```text
<ROOT>/archive.zip::inside.txt:1:1-7
    ZIPTODO
```

<!-- flag:encoding --><!-- flag:E -->
### `--encoding NAME`, `-E NAME`
Purpose: decode text before searching.
```sh
zrep -F -E utf-16le TODO <ROOT>/utf16.txt
```
```text
1:1-4
    TODO utf16
```

<!-- flag:passthru --><!-- flag:all-lines -->
### `--passthru`, `--all-lines`
Purpose: print every line and highlight matches.
```sh
zrep -F --passthru "TODO one" <ROOT>/a.txt
```
```text
1:7-14
    alpha TODO one
2:1
    beta two
```

<!-- flag:trim -->
### `--trim`
Purpose: trim leading whitespace in printed lines.
```sh
zrep -F --trim TODO <ROOT>/space.txt
```
```text
1:1-4
    TODO indented
```

<!-- flag:max-columns --><!-- flag:max-columns-preview -->
### `--max-columns N`, `--max-columns-preview`
Purpose: omit or preview very long matching lines.
```sh
zrep -F --max-columns 10 TODO <ROOT>/long.txt
```
```text
1:7-10
    [36 columns omitted]
```

<!-- flag:0 -->
### `-0`
Purpose: terminate path records with NUL.
```sh
zrep -F -l -0 TODO <ROOT>/a.txt
```
```text
<ROOT>/a.txt<NUL>
```

<!-- flag:path-separator -->
### `--path-separator SEP`
Purpose: rewrite printed path separators.
```sh
zrep --files <ROOT> --path-separator '|'
```
```text
<ROOT>|a.txt
```

<!-- flag:no-messages -->
### `--no-messages`
Purpose: suppress file read/open errors.
```sh
zrep -F --no-messages TODO <ROOT>/missing.txt
```
```text
stdout: empty
stderr: empty
```

<!-- flag:debug -->
### `--debug`
Purpose: print skip diagnostics.
```sh
zrep --debug --files <ROOT>
```
```text
stderr: zrep: debug: skip ...
```

<!-- flag:version -->
### `--version`
Purpose: print version and exit.
```sh
zrep --version
```
```text
zrep dev
```

<!-- flag:config --><!-- flag:no-config --><!-- flag:ZREP_CONFIG_PATH --><!-- flag:ZREP_NO_CONFIG -->
### `--config FILE`, `--no-config`, `ZREP_CONFIG_PATH`, `ZREP_NO_CONFIG`
Purpose: opt into or disable config loading.
```sh
ZREP_CONFIG_PATH=<ROOT>/zrep.conf zrep TODO <ROOT>
```
```text
<ROOT>/a.txt:3
```

<!-- flag:text -->
### `--text`
Purpose: search binary files as text.
```sh
zrep -F --text TODO <ROOT>/binary.bin
```
```text
1:8-11
    prefix<NUL>TODO binary
```

<!-- flag:hidden -->
### `--hidden`
Purpose: search hidden files and directories.
```sh
zrep -F --hidden -c TODO <ROOT> --include '*.txt' --sort path
```
```text
<ROOT>/.hidden/secret.txt:1
```

<!-- flag:no-ignore -->
### `--no-ignore`
Purpose: ignore `.gitignore`, `.ignore`, and `.zrepignore`.
```sh
zrep -F --no-ignore -c TODO <ROOT> --include '*.ignoreme'
```
```text
<ROOT>/ignored.ignoreme:1
```

<!-- flag:no-default-ignore -->
### `--no-default-ignore`
Purpose: search default skipped directories like `node_modules`.
```sh
zrep -F --no-default-ignore -c TODO <ROOT> --include '*.txt'
```
```text
<ROOT>/node_modules/pkg.txt:1
```

<!-- flag:include --><!-- flag:exclude --><!-- flag:g --><!-- flag:glob -->
### `--include`, `--exclude`, `-g`, `--glob`
Purpose: include or exclude path globs.
```sh
zrep -F --glob '*.log' TODO <ROOT>
```
```text
<ROOT>/sub/c.log:1:1-4
    TODO in log
```

<!-- flag:include-dir --><!-- flag:exclude-dir -->
### `--include-dir`, `--exclude-dir`
Purpose: include or exclude matching directories.
```sh
zrep -F --include-dir sub TODO <ROOT>
```
```text
<ROOT>/sub/c.log:1:1-4
    TODO in log
```

<!-- flag:t --><!-- flag:T --><!-- flag:type-add --><!-- flag:type-list --><!-- flag:Y -->
### `-t`, `-T`, `--type-add`, `--type-list`, `-Y`
Purpose: filter by file type or list known types.
```sh
zrep -F -t json TODO <ROOT>
```
```text
<ROOT>/data.json:1:8-11
    {"id":1,"login":"ada","user":{"name":"Ada"},"note":"TODO json"}
```

<!-- flag:sort --><!-- flag:sortr -->
### `--sort FIELD`, `--sortr FIELD`
Purpose: sort output by `path`, `modified`, or `size`.
```sh
zrep -F -c TODO <ROOT> --sort path
```
```text
<ROOT>/a.txt:2
<ROOT>/data.json:1
```

<!-- flag:inspect --><!-- flag:rows --><!-- flag:columns --><!-- flag:sample --><!-- flag:limit --><!-- flag:format --><!-- flag:max-cell-width --><!-- flag:cell-width --><!-- flag:select --><!-- flag:flatten --><!-- flag:where --><!-- flag:I --><!-- flag:R --><!-- flag:K --><!-- flag:N --><!-- flag:L -->
### Inspect flags
Purpose: inspect structured files, sample records, project fields, and format output.
```sh
zrep --inspect --select id,user.name --flatten --where login=ada --format table --limit 1 <ROOT>/data.json
```
```text
<ROOT>/data.json
kind     json
rows     1
columns  id, user.name
sample
id  user.name
1   Ada
```

<!-- flag:cpuprofile -->
### `--cpuprofile FILE`
Purpose: write a CPU profile while running.
```sh
zrep -F -c --cpuprofile <ROOT>/cpu.out TODO <ROOT>/a.txt
```
```text
2
```

<!-- flag:stats --><!-- flag:s -->
### `--stats`, `-s`
Purpose: print aggregate search stats to stderr.
```sh
zrep -F -s -c TODO <ROOT>/a.txt
```
```text
stdout: 2
stderr: zrep: files=1 bytes=...
```

<!-- flag:- -->
### `-` stdin path
Purpose: search standard input.
```sh
printf 'TODO via stdin\n' | zrep -F TODO -
```
```text
-:1:1-4
    TODO via stdin
```

Type matching is platform-agnostic: path separators are normalized and type
globs are matched case-insensitively, so `*.js` also matches `APP.JS` and both
Unix-style and Windows-style paths.

Data inspection does not require `-t`. zrep auto-detects CSV/TSV, JSON,
JSONL/NDJSON, and text from extension and light content sniffing, including
extensionless files.

Structured inspection can project fields and filter sampled records. JSON
fields are top-level by default; use `--flatten` for nested dot paths such as
`user.name`.

## Config Files

zrep can read flags from a config file so you do not need to remember the same
options for every search. Config loading is opt-in by default, so plain `zrep`
commands stay predictable in scripts and benchmarks.

Config can be enabled with:

```text
$ZREP_CONFIG_PATH
--config FILE
```

Use `--no-config` or `ZREP_NO_CONFIG=1` to force-disable config loading even
when `--config` or `ZREP_CONFIG_PATH` is present.

Config files use shell-like whitespace splitting, quotes, backslash escaping,
and `#` comments at the start of a token. Put one flag or command fragment per
line for readability:

```text
# ~/.config/zrep/work.conf
-F
-S
--hidden
--glob '!node_modules'
--glob '!dist'
--max-columns 240
--max-columns-preview
--sort path
```

CLI arguments are appended after config arguments, so command-line values for
scalar flags such as `--sort`, `--format`, or `--max-columns` override earlier
config values.

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
files listing
pattern-file count
files-without-match
count-matches
max-count
max-columns preview
passthru
glob alias count
sort path count
json-events
```

## Notes

`zrep` is not full ripgrep parity yet. PCRE2 is intentionally not implemented
in this pass because the Go standard library regexp engine does not support
look-around or backreferences without adding cross-platform dependency
complexity.

This project is currently optimizing the core search pipeline first.
