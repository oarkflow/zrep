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
zrep --profile code TODO .
zrep --config ~/.config/zrep/config.bcl TODO .
ZREP_CONFIG_PATH=~/.config/zrep/config.bcl zrep TODO .
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

## Testing Data Sets

zrep ships a hybrid testdata system:

- `testdata/` contains small, readable fixtures for examples, golden tests, and manual inspection.
- `internal/testdatafixture` generates deterministic CI-safe fixtures for large JSON, CSV, daily logs, SQL files, archives, encodings, binary files, ignore files, and BCL config profiles.
- `cmd/zrep-testdata` generates optional stress-scale data outside the repo for local benchmarking.

Small fixture examples:

```sh
zrep --inspect --sample 5 testdata/structured/users.json
zrep --logs --user-id 42 --activity login testdata/logs/
zrep --sql-files --sql-block object --sql-kind create users testdata/sql/
```

Copy-paste commands for the checked-in `testdata/` tree:

```sh
# Structured JSON, JSONL, CSV, and TSV inspection.
zrep --inspect --sample 5 testdata/structured/users.json
zrep --inspect --sample 3 --format pretty-json testdata/structured/users.json
zrep --inspect --select id,email,activity --format table testdata/structured/users.json
zrep --inspect --select id,user.name --flatten --format table testdata/structured/users.json
zrep --inspect --where email=ada@example.com --format table testdata/structured/users.json
zrep --rows testdata/structured/users.json
zrep --columns testdata/structured/users.json
zrep --schema testdata/structured/users.json
zrep --jq '.email==ada@example.com' testdata/structured/users.json

zrep --inspect --sample 3 testdata/structured/users.jsonl
zrep --inspect --sample 3 --format table testdata/structured/users.csv
zrep --inspect --sample 3 --format csv testdata/structured/users.csv
zrep --profile-data testdata/structured/users.csv
zrep --schema testdata/structured/users.csv
zrep --sql 'SELECT id,email WHERE activity=login' testdata/structured/users.csv
zrep --inspect --sample 3 testdata/structured/users.tsv
zrep --inspect --sample 2 --format table --max-cell-width 20 testdata/structured/wide.csv
zrep --inspect --select org.users --flatten --format table testdata/structured/nested.json
zrep --inspect --sample 3 testdata/structured/missing-null.json
zrep -F ZREP_NEEDLE testdata/structured/malformed.jsonl

# Plain search, path filters, hidden files, ignored files, and type filters.
zrep -F ZREP_NEEDLE testdata/search/
zrep -F -n ZREP_NEEDLE testdata/search/notes.txt
zrep -F -C 1 ZREP_NEEDLE testdata/search/notes.txt
zrep -F --count-matches ZREP_NEEDLE testdata/search/
zrep --files testdata/search/ --sort path
zrep --files testdata/search/ --hidden --sort path
zrep -F --hidden ZREP_NEEDLE testdata/search/
zrep -F --ignore-file testdata/config/zrep.ignore ZREP_NEEDLE testdata/search/
zrep -F --no-ignore ZREP_NEEDLE testdata/search/ignored/skip.log
zrep -F --glob '*.go' ZREP_NEEDLE testdata/search/
zrep -F -t go ZREP_NEEDLE testdata/search/
zrep -F -f testdata/config/patterns.txt testdata/search/

# Record-aware daily logs and multi-factor activity searches.
zrep --logs ERROR testdata/logs/
zrep --logs --user-id 42 testdata/logs/
zrep --logs --email ada@example.com testdata/logs/
zrep --logs --activity login testdata/logs/
zrep --logs --field service=api testdata/logs/
zrep --logs --field status=500 ERROR testdata/logs/
zrep --logs --from 2026-05-01 --to 2026-05-03 ERROR testdata/logs/
zrep --logs --since 1000000h ERROR testdata/logs/
zrep --logs --group-by service ERROR testdata/logs/
zrep --logs --histogram hour ERROR testdata/logs/
zrep --logs --query 'status = 500 AND service = "billing"' testdata/logs/
zrep --logs --xql 'user_id = 42 AND activity = "login"' testdata/logs/
zrep --logs --xql 'zrep_records | where status == 500 | select __zrep_index' testdata/logs/
zrep --logs --format table --select timestamp,level,service,user_id,email,activity,status testdata/logs/activity.log
zrep --logs --format json --email ada@example.com testdata/logs/2026-05-02/app.jsonl
zrep --logs --ip 10.0.0.1 testdata/logs/mixed.log

# SQL file search and block extraction.
zrep --sql-files user_id testdata/sql/
zrep --sql-files --sql-block statement user_id testdata/sql/migration.sql
zrep --sql-files --sql-block context --sql-context 2 user_id testdata/sql/migration.sql
zrep --sql-files --sql-block object --sql-kind create users testdata/sql/
zrep --sql-files --sql-kind insert ZREP_NEEDLE testdata/sql/
zrep --sql-files --format json user_id testdata/sql/
zrep --sql-files --xql 'kind == "create" AND table == "users"' testdata/sql/

# Config profiles and pattern/config examples.
zrep --config testdata/config/zrep.bcl ZREP_NEEDLE testdata/search/
zrep --config testdata/config/zrep.bcl --profile code ZREP_NEEDLE testdata/search/
zrep --config testdata/config/zrep.bcl --config-id logs ERROR testdata/logs/
ZREP_CONFIG_PATH=testdata/config/zrep.bcl ZREP_PROFILE=code zrep ZREP_NEEDLE testdata/search/

# Generated binary, UTF-16, gzip, zip, and larger stress fixtures.
go run ./cmd/zrep-testdata -root /tmp/zrep-testdata -rows 10000 -days 14
zrep -F --text ZREP_NEEDLE /tmp/zrep-testdata/search/binary.bin
zrep -F --encoding utf-16le ZREP_NEEDLE /tmp/zrep-testdata/search/utf16.txt
zrep -F --search-compressed ERROR /tmp/zrep-testdata/archives/app.log.gz
zrep -F --search-archives ZREP_NEEDLE /tmp/zrep-testdata/archives/bundle.zip
zrep --inspect --sample 5 /tmp/zrep-testdata/structured/users.json
zrep --logs --user-id 42 --activity login /tmp/zrep-testdata/logs/
```

Generate a larger local corpus:

```sh
go run ./cmd/zrep-testdata -root /tmp/zrep-testdata -rows 10000 -days 14
zrep -F --search-compressed ERROR /tmp/zrep-testdata/archives/app.log.gz
```

Generate stress fixtures through tests:

```sh
go test ./internal/testdatafixture -run TestGenerateStressCorpus -stress-root /tmp/zrep-testdata
ZREP_STRESS_TESTDATA=1 ZREP_STRESS_ROWS=100000 go test ./internal/testdatafixture -run TestGenerateStressCorpus
```

The stress generator prints a manifest with each file path, type, row count,
byte size, and example zrep commands.

## Use Cases

### Code Search In A Monorepo

Find TODOs in source files while skipping generated and dependency folders:

```sh
zrep -F TODO . \
  --glob '*.go' \
  --glob '*.ts' \
  --glob '!*.gen.go' \
  --exclude-dir node_modules \
  --exclude-dir vendor \
  --sort path
```

Show context around a function or symbol:

```sh
zrep -C 3 'func Handle[A-Za-z]+' internal/
```

Use smart case for daily code search:

```sh
zrep -S todo .
zrep -S TODO .
```

### Editor And CI Integration

Emit editor-friendly `file:line:column:text` output:

```sh
zrep -F --vimgrep TODO .
```

Emit machine-readable JSON events for scripts:

```sh
zrep -F --json-events ERROR logs/
```

Fail fast in CI if a forbidden pattern exists:

```sh
zrep -F -q 'console.log' src/
```

### Large Files And Dumps

Search a huge file without loading it all into memory:

```sh
zrep -F 'customer_id' ~/Downloads/large-file.json
```

Skip files that are too large for the current job:

```sh
zrep -F --max-filesize 500M ERROR dumps/
```

Search binary-ish dumps as text when you know that is intentional:

```sh
zrep -F --text NEEDLE dumps/
```

### Logs And Compressed Logs

Search compressed logs:

```sh
zrep -F --search-compressed ERROR logs/app.log.gz
zrep -F --search-compressed ERROR archives/
```

Group matching log lines by a `service=value` field:

```sh
zrep --logs --group-by service ERROR logs/
```

Build an hourly histogram of matching log lines:

```sh
zrep --logs --histogram hour ERROR logs/
```

Filter timestamped logs:

```sh
zrep --logs --since 2h ERROR logs/
zrep --logs --from 2026-05-01 --to 2026-05-31 ERROR logs/
```

### Structured Data Exploration

Inspect CSV, JSON, JSONL, or TSV without specifying a type:

```sh
zrep --inspect --sample 5 data/
```

Print a discovered schema:

```sh
zrep --schema data.json
zrep --schema data.csv
```

Profile a CSV for nulls, unique values, and duplicates:

```sh
zrep --profile-data users.csv
```

Project and filter sampled structured data:

```sh
zrep --inspect --select id,user.login --flatten --where active=true --format table users.json
```

Use lightweight JSON and CSV query helpers:

```sh
zrep --jq '.login==ada' users.json
zrep --sql 'SELECT id,login WHERE active=true' users.csv
```

### Advanced Pattern Matching

Use pure-Go advanced regex compatibility mode for lookaround and backreferences:

```sh
zrep -P '(?<=user=)\d+' app.log
zrep -P '(foo)(bar)\1' data.txt
```

Use fuzzy matching for misspellings in logs or text:

```sh
zrep --fuzzy --distance 2 authrization logs/
```

Use boolean term search:

```sh
zrep --boolean '(error OR fatal) AND timeout' logs/
```

### Config Profiles For Repeated Workflows

Create and use a code-search profile:

```sh
zrep profile import code -- -F --sort path --glob '*.go' --glob '*.ts' --exclude-dir node_modules
zrep --profile code TODO .
```

Create and use a large-JSON inspection profile:

```sh
zrep profile import large_json -- --inspect --format table --max-cell-width 80
zrep --profile large_json ~/Downloads/large-file.json
```

Update or remove a profile:

```sh
zrep profile update code -- -F --sort path --glob '*.go'
zrep profile remove code
```

### Replacement Preview And Safe Writes

Preview replacements without editing files:

```sh
zrep -F --replace DONE TODO src/
```

Apply replacements to a specific file:

```sh
zrep -F --replace DONE --write TODO src/task.txt
```

### Path Discovery And Auditing

List searchable files after filters:

```sh
zrep --files . --sort path
zrep --files . --include '*.go' --exclude-dir vendor
```

Find files that do not contain a required marker:

```sh
zrep -F --files-without-match 'SPDX-License-Identifier' .
```

Follow symlinked source folders:

```sh
zrep -L TODO linked-src/
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
-profile ID  apply a named BCL config profile
-config-id ID alias for -profile
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
-P  alias for -pcre2
-L  alias for -follow
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

<!-- flag:pretty -->
### `--pretty`
Purpose: force pretty block output.
```sh
zrep -F --pretty "TODO one" <ROOT>/a.txt
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

<!-- flag:config --><!-- flag:profile --><!-- flag:config-id --><!-- flag:no-config --><!-- flag:ZREP_CONFIG_PATH --><!-- flag:ZREP_PROFILE --><!-- flag:ZREP_NO_CONFIG -->
### `--config FILE`, `--profile ID`, `--config-id ID`, `--no-config`, `ZREP_CONFIG_PATH`, `ZREP_PROFILE`, `ZREP_NO_CONFIG`
Purpose: load config flags, apply named BCL profiles, or disable config loading.
```sh
zrep --profile code TODO <ROOT>
```
```text
<ROOT>/sub/c.log:1:1-4
    TODO in log
```

<!-- flag:pcre2 --><!-- flag:P --><!-- flag:fuzzy --><!-- flag:distance --><!-- flag:boolean --><!-- flag:semantic -->
### `-P`, `--pcre2`, `--fuzzy`, `--distance N`, `--boolean`, `--semantic`
Purpose: opt into advanced pure-Go match engines.
```sh
zrep -P '(?<=user=)\d+' <ROOT>/advanced.txt
zrep --fuzzy --distance 2 authrization <ROOT>/advanced.txt
zrep --boolean '(error OR fatal) AND timeout' <ROOT>/advanced.txt
```
```text
1:6-8
    user=42 action=login
```

<!-- flag:follow --><!-- flag:L --><!-- flag:max-filesize --><!-- flag:ignore-file --><!-- flag:search-compressed -->
### `-L`, `--follow`, `--max-filesize SIZE`, `--ignore-file FILE`, `--search-compressed`
Purpose: control path discovery, symlink traversal, large-file skipping, extra ignore files, and compressed input.
```sh
zrep -L TODO linked-dir
zrep --max-filesize 100M TODO .
zrep --ignore-file custom.ignore TODO .
zrep --search-compressed ERROR logs.gz
```
```text
matching lines from followed, allowed, and compressed inputs
```

<!-- flag:schema --><!-- flag:profile-data --><!-- flag:jq --><!-- flag:sql --><!-- flag:output -->
### `--schema`, `--profile-data`, `--jq EXPR`, `--sql QUERY`, `--output FORMAT`
Purpose: inspect, filter, and transform structured local data.
```sh
zrep --schema <ROOT>/data.json
zrep --profile-data <ROOT>/data.csv
zrep --jq '.login==ada' <ROOT>/data.json
zrep --sql 'SELECT id,login WHERE login=bob' <ROOT>/data.csv
```
```text
schema, profile rows, or filtered records
```

<!-- flag:logs --><!-- flag:since --><!-- flag:from --><!-- flag:to --><!-- flag:group-by --><!-- flag:histogram -->
### `--logs`, `--since DURATION`, `--from TIME`, `--to TIME`, `--group-by FIELD`, `--histogram UNIT`
Purpose: summarize timestamped log matches.
```sh
zrep --logs --group-by service ERROR logs/
zrep --logs --histogram hour ERROR logs/
```
```text
api 121
worker 55
```

<!-- flag:activity --><!-- flag:user-id --><!-- flag:email --><!-- flag:ip --><!-- flag:session-id --><!-- flag:request-id --><!-- flag:field --><!-- flag:query --><!-- flag:xql -->
### `--activity`, `--user-id`, `--email`, `--ip`, `--session-id`, `--request-id`, `--field`, `--query`, `--xql`
Purpose: search log records by multiple extracted factors.
```sh
zrep --logs --user-id 42 --activity login logs/daily/
zrep --logs --email ada@example.com --format table --select user_id,email,activity logs.jsonl
zrep --logs --field service=api --query 'status = 500' ERROR logs/
zrep --logs --xql 'user_id = 42 AND activity = "checkout"' logs/
zrep --logs --xql 'zrep_records | where status == 500 | select __zrep_index' logs/
```
```text
logs/app.log:12:1
    2026-05-01T10:00:00Z INFO service=api user_id=42 activity=login
```
`--xql` runs through the local `github.com/oarkflow/xql` engine. A short expression is wrapped as `zrep_records | where ... | select __zrep_index`; full XQL pipelines can use the `zrep_records` source directly.

<!-- flag:sql-files --><!-- flag:sql-block --><!-- flag:sql-context --><!-- flag:sql-kind -->
### `--sql-files`, `--sql-block MODE`, `--sql-context N`, `--sql-kind KIND`
Purpose: search `.sql` files and return full SQL blocks.
```sh
zrep --sql-files customer_id migrations/
zrep --sql-files --sql-block statement user_id schema.sql
zrep --sql-files --sql-block context --sql-context 2 customer_id schema.sql
zrep --sql-files --sql-block object --sql-kind create users schema.sql
```
```text
schema.sql:1-5
    CREATE TABLE users (
      id INTEGER,
      user_id INTEGER
    );
```

<!-- flag:watch --><!-- flag:tui -->
### `--watch`, `--tui`
Purpose: reserved pure-Go live and interactive modes.
```sh
zrep --watch TODO logs/
zrep --tui TODO .
```
```text
zrep reports that the mode is reserved until the interactive implementation is enabled.
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

<!-- flag:inspect --><!-- flag:rows --><!-- flag:columns --><!-- flag:sample --><!-- flag:limit --><!-- flag:format --><!-- flag:max-cell-width --><!-- flag:cell-width --><!-- flag:select --><!-- flag:flatten --><!-- flag:where --><!-- flag:I --><!-- flag:R --><!-- flag:K --><!-- flag:N -->
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
options for every search. Config loading is still off for plain `zrep` commands,
so scripts and benchmarks stay predictable.

Config can be enabled with:

```text
$ZREP_CONFIG_PATH
--config FILE
--profile ID
--config-id ID
ZREP_PROFILE=ID
```

When a profile is selected without `--config` or `ZREP_CONFIG_PATH`, zrep reads
the first existing default BCL config from:

```text
$XDG_CONFIG_HOME/zrep/config.bcl
~/.config/zrep/config.bcl
~/.zrep.bcl
```

Use `--no-config` or `ZREP_NO_CONFIG=1` to force-disable config loading even
when `--config`, `ZREP_CONFIG_PATH`, or a profile selector is present.

`.bcl` is the recommended config format. It supports global flags plus named
profiles that can be selected with `--profile ID`, `--config-id ID`, or
`ZREP_PROFILE=ID`.

```bcl
global ["-F", "--sort", "path", "--max-columns", "240", "--max-columns-preview"]

profiles {
  code ["--glob", "*.go", "--glob", "*.ts", "--exclude-dir", "node_modules"]
  large_json ["--inspect", "--format", "table", "--max-cell-width", "80"]
  logs ["-F", "--passthru", "--max-count", "20"]
}
```

CLI arguments are appended after config arguments, so command-line values for
scalar flags such as `--sort`, `--format`, or `--max-columns` override earlier
config values.

Application order:

```text
global config flags
selected profile flags
CLI flags
```

Examples:

```sh
# Use the default BCL config profile.
zrep --profile code TODO .

# Global BCL config only from an explicit file.
zrep --config ~/.config/zrep/config.bcl TODO .

# Equivalent profile selector.
zrep --config-id large_json ~/Downloads/large-file.json

# Select profile through the environment.
ZREP_PROFILE=logs zrep ERROR logs/

# Select profile from an explicit config path.
ZREP_CONFIG_PATH=~/.config/zrep/team.bcl ZREP_PROFILE=logs zrep ERROR logs/
```

Manage profiles with the `profile` subcommand. Use `--config FILE` to edit a
specific BCL file, or omit it to edit the default BCL config path.

```sh
# Add a new profile. `--` keeps the profile flags from being parsed as zrep command flags.
zrep profile import code -- -F --glob '*.go' --exclude-dir node_modules

# Replace an existing profile.
zrep profile update code -- -F --glob '*.go' --glob '*.ts'

# Show profile IDs.
zrep profile list

# Remove a profile.
zrep profile remove code
```

Responses:

```text
imported profile "code" into ~/.config/zrep/config.bcl
updated profile "code" in ~/.config/zrep/config.bcl
code
removed profile "code" from ~/.config/zrep/config.bcl
```

If a selected profile does not exist, zrep exits with code `2`:

```text
zrep: config profile "missing" not found in ~/.config/zrep/config.bcl
```

If a profile is selected but no default BCL config exists, zrep exits with code
`2` and asks for a default config or `--config`:

```text
zrep: config profile "code" requested but no default config was found; create ...
```

Non-BCL config files still use the legacy shell-like whitespace splitting,
quotes, backslash escaping, and `#` comments at the start of a token. Profile
selection is ignored for legacy flat config files. Prefer `.bcl` for new
configuration; legacy flat files can use any non-`.bcl` extension such as
`.flags` or `.txt`.

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
max-filesize count
advanced regex
compressed gzip
fuzzy count
schema csv
```

## Notes

`zrep` uses a pure-Go advanced regex engine for `-P/--pcre2`. It supports many
PCRE-style constructs such as lookaround and backreferences, but it is not the
native PCRE2 C library.

This project is currently optimizing the core search pipeline first.
