package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

type flagFixture struct {
	root      string
	files     map[string]string
	config    string
	bclConfig string
	xdg       string
	pattern   string
}

type zrepRun struct {
	stdout string
	stderr string
	code   int
}

type flagExampleCase struct {
	name           string
	args           []string
	stdin          string
	env            []string
	flags          []string
	wantStdout     string
	wantStdoutHas  []string
	wantStderr     string
	wantStderrHas  []string
	wantExit       int
	normalizeNulls bool
}

var registerFlagsOnce sync.Once
var buildZrepOnce sync.Once
var builtZrepPath string
var builtZrepErr error

func TestFlagExamples(t *testing.T) {
	fx := newFlagFixture(t)
	cases := []flagExampleCase{
		{name: "fixed string", flags: []string{"F", "no-color"}, args: []string{"-F", "-no-color", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "regexp", flags: []string{"n"}, args: []string{"-n", "TODO [a-z]+", fx.path("a.txt")}, wantStdout: "1:7-14\n    alpha TODO one\n3:12-21\n    alpha TODO TODO three\n"},
		{name: "pattern e", flags: []string{"e"}, args: []string{"-F", "-e", "TODO", "-e", "FIXME", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n4:1-5\n    FIXME item\n"},
		{name: "pattern file", flags: []string{"f", "file"}, args: []string{"-F", "-f", fx.pattern, "-c", fx.root, "--include-zero", "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/a.txt:3\n", "<ROOT>/b.log:0\n", "<ROOT>/patterns.txt:2\n"}},
		{name: "ignore case", flags: []string{"i"}, args: []string{"-F", "-i", "todo", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "smart case", flags: []string{"S"}, args: []string{"-F", "-S", "todo", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "invert", flags: []string{"v"}, args: []string{"-F", "-v", "TODO", fx.path("a.txt")}, wantStdout: "2:1\n    beta two\n4:1\n    FIXME item\n5:1\n    \n"},
		{name: "word", flags: []string{"w"}, args: []string{"-F", "-w", "TODO", fx.path("words.txt")}, wantStdout: "1:1-4\n    TODO todoish\n"},
		{name: "line", flags: []string{"x"}, args: []string{"-F", "-x", "TODO", fx.path("lines.txt")}, wantStdout: "1:1-4\n    TODO\n"},
		{name: "only matching", flags: []string{"o"}, args: []string{"-F", "-o", "TODO", fx.path("a.txt")}, wantStdout: "TODO\nTODO\nTODO\n"},
		{name: "files with matches", flags: []string{"l"}, args: []string{"-F", "-l", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/a.txt\n", "<ROOT>/lines.txt\n"}},
		{name: "files without match", flags: []string{"files-without-match", "without-match"}, args: []string{"-F", "--without-match", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/b.log\n", "<ROOT>/zrep.flags\n"}},
		{name: "files listing", flags: []string{"files", "list-files"}, args: []string{"--list-files", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/a.txt\n", "<ROOT>/data.json\n"}},
		{name: "count include zero", flags: []string{"c", "include-zero"}, args: []string{"-F", "-c", "TODO", fx.root, "--include-zero", "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/a.txt:2\n", "<ROOT>/b.log:0\n"}},
		{name: "count matches", flags: []string{"count-matches", "matches-count"}, args: []string{"-F", "--matches-count", "TODO", fx.path("a.txt")}, wantStdout: "3\n"},
		{name: "max count", flags: []string{"m", "max-count"}, args: []string{"-F", "--max-count", "1", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n"},
		{name: "quiet match", flags: []string{"q", "quiet"}, args: []string{"-F", "-q", "TODO", fx.path("a.txt")}, wantExit: 0},
		{name: "quiet no match", flags: []string{"q"}, args: []string{"-F", "-q", "NOPE", fx.path("a.txt")}, wantExit: 1},
		{name: "line numbers", flags: []string{"n"}, args: []string{"-F", "-n", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "with filename", flags: []string{"H"}, args: []string{"-F", "-H", "TODO", fx.path("a.txt")}, wantStdout: "<ROOT>/a.txt:1:7-10\n    alpha TODO one\n<ROOT>/a.txt:3:7-10\n    alpha TODO TODO three\n"},
		{name: "no filename", flags: []string{"h"}, args: []string{"-F", "-h", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"1:7-10\n    alpha TODO one\n"}},
		{name: "workers", flags: []string{"j"}, args: []string{"-F", "-j", "1", "-c", "TODO", fx.path("a.txt")}, wantStdout: "2\n"},
		{name: "after context", flags: []string{"A"}, args: []string{"-F", "-A", "1", "TODO one", fx.path("a.txt")}, wantStdout: "1:7-14\n    alpha TODO one\n2:1\n    beta two\n"},
		{name: "before context", flags: []string{"B"}, args: []string{"-F", "-B", "1", "TODO TODO", fx.path("a.txt")}, wantStdout: "2:1\n    beta two\n3:7-15\n    alpha TODO TODO three\n"},
		{name: "context", flags: []string{"C"}, args: []string{"-F", "-C", "1", "TODO TODO", fx.path("a.txt")}, wantStdout: "2:1\n    beta two\n3:7-15\n    alpha TODO TODO three\n4:1\n    FIXME item\n"},
		{name: "pcre2", flags: []string{"pcre2", "P"}, args: []string{"-P", `(?<=user=)\d+`, fx.path("advanced.txt")}, wantStdout: "1:6-7\n    user=42 action=login\n"},
		{name: "fuzzy", flags: []string{"fuzzy", "distance"}, args: []string{"--fuzzy", "--distance", "2", "authrization", fx.path("advanced.txt")}, wantStdoutHas: []string{"authorization"}},
		{name: "semantic", flags: []string{"semantic"}, args: []string{"--semantic", "authrization", fx.path("advanced.txt")}, wantStdoutHas: []string{"authorization"}},
		{name: "boolean", flags: []string{"boolean"}, args: []string{"--boolean", "(error OR fatal) AND timeout", fx.path("advanced.txt")}, wantStdoutHas: []string{"fatal timeout"}},
		{name: "json", flags: []string{"json", "J"}, args: []string{"-F", "-J", "TODO one", fx.path("a.txt")}, wantStdout: "{\"path\":\"<ROOT>/a.txt\",\"line\":1,\"column\":7,\"end_column\":14,\"context\":false,\"text\":\"alpha TODO one\"}\n"},
		{name: "json events", flags: []string{"json-events"}, args: []string{"-F", "--json-events", "TODO one", fx.path("a.txt")}, wantStdoutHas: []string{`"type":"begin"`, `"type":"match"`, `"type":"end"`, `"type":"summary"`}},
		{name: "vimgrep", flags: []string{"vimgrep", "G"}, args: []string{"-F", "-G", "TODO one", fx.path("a.txt")}, wantStdout: "<ROOT>/a.txt:1:7:alpha TODO one\n"},
		{name: "heading", flags: []string{"heading"}, args: []string{"-F", "-H", "--heading", "TODO", fx.path("a.txt")}, wantStdout: "<ROOT>/a.txt\n1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "pretty", flags: []string{"pretty"}, args: []string{"-F", "--pretty", "TODO one", fx.path("a.txt")}, wantStdout: "1:7-14\n    alpha TODO one\n"},
		{name: "replace", flags: []string{"replace", "r"}, args: []string{"-F", "-r", "DONE", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha DONE one\n3:7-10\n    alpha DONE DONE three\n"},
		{name: "write", flags: []string{"write", "W"}, args: []string{"-F", "--replace", "DONE", "-W", "TODO", fx.copyPath(t, "write.txt", "TODO\n")}},
		{name: "multiline", flags: []string{"multiline", "M"}, args: []string{"-M", "start(.|\n)*end", fx.path("multi.txt")}, wantStdout: "1:1-21\n    start\nmiddle TODO\nend\n"},
		{name: "archive", flags: []string{"search-archives", "Z"}, args: []string{"-F", "-H", "-Z", "ZIPTODO", fx.path("archive.zip")}, wantStdout: "<ROOT>/archive.zip::inside.txt:1:1-7\n    ZIPTODO\n"},
		{name: "compressed", flags: []string{"search-compressed"}, args: []string{"-F", "-H", "--search-compressed", "GZTODO", fx.path("archive.gz")}, wantStdout: "<ROOT>/archive.gz:1:1-6\n    GZTODO\n"},
		{name: "encoding", flags: []string{"encoding", "E"}, args: []string{"-F", "-E", "utf-16le", "TODO", fx.path("utf16.txt")}, wantStdout: "1:1-4\n    TODO utf16\n"},
		{name: "passthru", flags: []string{"passthru", "all-lines"}, args: []string{"-F", "--all-lines", "TODO one", fx.path("a.txt")}, wantStdout: "1:7-14\n    alpha TODO one\n2:1\n    beta two\n3:1\n    alpha TODO TODO three\n4:1\n    FIXME item\n"},
		{name: "trim", flags: []string{"trim"}, args: []string{"-F", "--trim", "TODO", fx.path("space.txt")}, wantStdout: "1:1-4\n    TODO indented\n"},
		{name: "max columns omit", flags: []string{"max-columns"}, args: []string{"-F", "--max-columns", "10", "TODO", fx.path("long.txt")}, wantStdout: "1:8-11\n    [37 columns omitted]\n"},
		{name: "max columns preview", flags: []string{"max-columns-preview"}, args: []string{"-F", "--max-columns", "12", "--max-columns-preview", "TODO", fx.path("long.txt")}, wantStdout: "1:8-11\n    prefix TODO \n"},
		{name: "null", flags: []string{"0"}, args: []string{"-F", "-l", "-0", "TODO", fx.path("a.txt")}, wantStdout: "<ROOT>/a.txt<NUL>", normalizeNulls: true},
		{name: "path separator", flags: []string{"path-separator"}, args: []string{"--files", fx.root, "--sort", "path", "--path-separator", "|"}, wantStdoutHas: []string{"<ROOT>|a.txt\n"}},
		{name: "no messages", flags: []string{"no-messages"}, args: []string{"-F", "--no-messages", "TODO", fx.path("missing.txt")}},
		{name: "debug", flags: []string{"debug"}, args: []string{"--debug", "--files", fx.root, "--sort", "path"}, wantStderrHas: []string{"zrep: debug: skip"}},
		{name: "version", flags: []string{"version"}, args: []string{"--version"}, wantStdout: "zrep dev\n"},
		{name: "follow symlink", flags: []string{"follow", "L"}, args: []string{"-F", "-L", "LINKTODO", fx.path("link-dir"), "--sort", "path"}, wantStdoutHas: []string{"LINKTODO"}},
		{name: "max filesize", flags: []string{"max-filesize"}, args: []string{"--files", "--max-filesize", "10", "--include", "a.txt", fx.root}, wantStdout: ""},
		{name: "ignore file", flags: []string{"ignore-file"}, args: []string{"-F", "--ignore-file", fx.path("custom.ignore"), "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/lines.txt"}, wantStdout: ""},
		{name: "config", flags: []string{"config"}, env: []string{"ZREP_NO_CONFIG=0"}, args: []string{"--config", fx.config, "TODO", fx.root}, wantStdout: "<ROOT>/a.txt:3\n"},
		{name: "no config", flags: []string{"no-config"}, env: []string{"ZREP_CONFIG_PATH=" + fx.config, "ZREP_NO_CONFIG=0"}, args: []string{"--no-config", "-F", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "env config", flags: []string{"ZREP_CONFIG_PATH"}, env: []string{"ZREP_CONFIG_PATH=" + fx.config, "ZREP_NO_CONFIG=0"}, args: []string{"TODO", fx.root}, wantStdout: "<ROOT>/a.txt:3\n"},
		{name: "env no config", flags: []string{"ZREP_NO_CONFIG"}, env: []string{"ZREP_CONFIG_PATH=" + fx.config, "ZREP_NO_CONFIG=1"}, args: []string{"-F", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "bcl global config", flags: []string{"profile"}, env: []string{"ZREP_NO_CONFIG=0"}, args: []string{"--config", fx.bclConfig, "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n3:7-10\n    alpha TODO TODO three\n"},
		{name: "bcl profile", flags: []string{"profile"}, env: []string{"ZREP_NO_CONFIG=0"}, args: []string{"--config", fx.bclConfig, "--profile", "logs", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n"},
		{name: "bcl config id", flags: []string{"config-id"}, env: []string{"ZREP_NO_CONFIG=0"}, args: []string{"--config", fx.bclConfig, "--config-id", "code", "TODO", fx.root}, wantStdout: "<ROOT>/sub/c.log:1:1-4\n    TODO in log\n"},
		{name: "bcl env profile", flags: []string{"ZREP_PROFILE"}, env: []string{"ZREP_CONFIG_PATH=" + fx.bclConfig, "ZREP_PROFILE=code", "ZREP_NO_CONFIG=0"}, args: []string{"TODO", fx.root}, wantStdout: "<ROOT>/sub/c.log:1:1-4\n    TODO in log\n"},
		{name: "bcl default profile", flags: []string{"profile"}, env: []string{"XDG_CONFIG_HOME=" + fx.xdg, "ZREP_NO_CONFIG=0"}, args: []string{"--profile", "logs", "TODO", fx.path("a.txt")}, wantStdout: "1:7-10\n    alpha TODO one\n"},
		{name: "bcl default config id", flags: []string{"config-id"}, env: []string{"XDG_CONFIG_HOME=" + fx.xdg, "ZREP_NO_CONFIG=0"}, args: []string{"--config-id", "code", "TODO", fx.root}, wantStdout: "<ROOT>/sub/c.log:1:1-4\n    TODO in log\n"},
		{name: "bcl missing profile", flags: []string{"profile"}, env: []string{"ZREP_NO_CONFIG=0"}, args: []string{"--config", fx.bclConfig, "--profile", "missing", "TODO", fx.root}, wantExit: 2, wantStderrHas: []string{"config profile \"missing\" not found"}},
		{name: "text binary", flags: []string{"text"}, args: []string{"-F", "--text", "TODO", fx.path("binary.bin")}, wantStdoutHas: []string{"binary"}},
		{name: "hidden", flags: []string{"hidden"}, args: []string{"-F", "--hidden", "-c", "TODO", fx.root, "--include", "*.txt", "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/.hidden/secret.txt:1\n"}},
		{name: "no ignore", flags: []string{"no-ignore"}, args: []string{"-F", "--no-ignore", "-c", "TODO", fx.root, "--include", "*.ignoreme", "--sort", "path"}, wantStdout: "<ROOT>/ignored.ignoreme:1\n"},
		{name: "no default ignore", flags: []string{"no-default-ignore"}, args: []string{"-F", "--no-default-ignore", "-c", "TODO", fx.root, "--include", "*.txt", "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/node_modules/pkg.txt:1\n"}},
		{name: "include", flags: []string{"include"}, args: []string{"-F", "--include", "*.log", "TODO", fx.root, "--sort", "path"}, wantStdout: "<ROOT>/sub/c.log:1:1-4\n    TODO in log\n"},
		{name: "exclude", flags: []string{"exclude"}, args: []string{"-F", "--exclude", "a.txt", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/lines.txt"}, wantStdout: ""},
		{name: "glob include", flags: []string{"g", "glob"}, args: []string{"-F", "-g", "*.log", "TODO", fx.root, "--sort", "path"}, wantStdout: "<ROOT>/sub/c.log:1:1-4\n    TODO in log\n"},
		{name: "glob exclude", flags: []string{"g"}, args: []string{"-F", "--glob", "!a.txt", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/lines.txt"}},
		{name: "include dir", flags: []string{"include-dir"}, args: []string{"-F", "--include-dir", "sub", "TODO", fx.root, "--sort", "path"}, wantStdout: "<ROOT>/sub/c.log:1:1-4\n    TODO in log\n"},
		{name: "exclude dir", flags: []string{"exclude-dir"}, args: []string{"-F", "--exclude-dir", "sub", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/a.txt"}},
		{name: "type include", flags: []string{"t"}, args: []string{"-F", "-t", "json", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/data.json:1:", "<ROOT>/data.jsonl:1:"}},
		{name: "type exclude", flags: []string{"T"}, args: []string{"-F", "-T", "text", "TODO", fx.root, "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/data.json"}},
		{name: "type add", flags: []string{"type-add"}, args: []string{"-F", "--type-add", "mine:*.mine", "-t", "mine", "TODO", fx.root, "--sort", "path"}, wantStdout: "<ROOT>/custom.mine:1:1-4\n    TODO custom\n"},
		{name: "type list", flags: []string{"type-list", "Y"}, args: []string{"-Y"}, wantStdoutHas: []string{"go", "json", "windows"}},
		{name: "sort", flags: []string{"sort"}, args: []string{"-F", "-c", "TODO", fx.root, "--include", "*.txt", "--sort", "path"}, wantStdoutHas: []string{"<ROOT>/a.txt:2\n"}},
		{name: "sortr", flags: []string{"sortr"}, args: []string{"-F", "-c", "TODO", fx.root, "--include", "*.txt", "--sortr", "path"}, wantStdoutHas: []string{"<ROOT>/words.txt:1\n"}},
		{name: "inspect plain", flags: []string{"inspect"}, args: []string{"--inspect", "--sample", "1", fx.path("data.csv")}, wantStdoutHas: []string{"kind: csv", "rows: 3", "sample:"}},
		{name: "schema", flags: []string{"schema"}, args: []string{"--schema", fx.path("data.json")}, wantStdoutHas: []string{"user.name string"}},
		{name: "profile data", flags: []string{"profile-data"}, args: []string{"--profile-data", fx.path("data.csv")}, wantStdoutHas: []string{"rows: 2", "login nulls=0"}},
		{name: "jq", flags: []string{"jq"}, args: []string{"--jq", ".login==ada", fx.path("data.json")}, wantStdoutHas: []string{`"login":"ada"`}},
		{name: "sql", flags: []string{"sql"}, args: []string{"--sql", "SELECT id,login WHERE login=bob", fx.path("data.csv")}, wantStdout: "2,bob\n"},
		{name: "output", flags: []string{"output"}, args: []string{"--inspect", "--sample", "1", "--output", "json", fx.path("data.csv")}, wantStdoutHas: []string{`"kind":"csv"`}},
		{name: "logs group", flags: []string{"logs", "group-by", "since", "from", "to"}, args: []string{"--logs", "--group-by", "service", "--since", "1000000h", "--from", "2026-01-01", "--to", "2026-12-31", "ERROR", fx.path("logs.txt")}, wantStdout: "api 1\nworker 1\n"},
		{name: "logs histogram", flags: []string{"histogram"}, args: []string{"--logs", "--histogram", "hour", "ERROR", fx.path("logs.txt")}, wantStdoutHas: []string{"2026-05-01 10:00 2"}},
		{name: "watch reserved", flags: []string{"watch"}, args: []string{"--watch", "TODO", fx.path("a.txt")}, wantExit: 2, wantStderrHas: []string{"--watch is reserved"}},
		{name: "tui reserved", flags: []string{"tui"}, args: []string{"--tui", "TODO", fx.path("a.txt")}, wantExit: 2, wantStderrHas: []string{"--tui is reserved"}},
		{name: "rows columns", flags: []string{"rows", "columns", "R", "K"}, args: []string{"-R", "-K", fx.path("data.csv")}, wantStdoutHas: []string{"rows: 3", "columns: id, login, note"}},
		{name: "sample limit", flags: []string{"sample", "limit", "N"}, args: []string{"--inspect", "--limit", "1", fx.path("data.json")}, wantStdoutHas: []string{"sample:"}},
		{name: "format json", flags: []string{"format"}, args: []string{"--inspect", "--sample", "1", "--format", "json", fx.path("data.json")}, wantStdoutHas: []string{`"kind":"json"`, `"sample"`}},
		{name: "format pretty json", flags: []string{"format"}, args: []string{"--inspect", "--sample", "1", "--format", "pretty-json", fx.path("data.json")}, wantStdoutHas: []string{"{\n", "  \"kind\": \"json\""}},
		{name: "format table", flags: []string{"format"}, args: []string{"--inspect", "--sample", "1", "--format", "table", fx.path("data.csv")}, wantStdoutHas: []string{"sample\n", "id  login"}},
		{name: "format csv", flags: []string{"format"}, args: []string{"--inspect", "--sample", "1", "--format", "csv", fx.path("data.csv")}, wantStdoutHas: []string{"path,kind,rows,data_rows,columns"}},
		{name: "cell width", flags: []string{"max-cell-width", "cell-width"}, args: []string{"--inspect", "--sample", "1", "--format", "table", "--cell-width", "8", fx.path("data.json")}, wantStdoutHas: []string{"TODO ..."}},
		{name: "select flatten where", flags: []string{"select", "flatten", "where"}, args: []string{"--inspect", "--select", "id,user.name", "--flatten", "--where", "login=ada", "--format", "table", "--limit", "1", fx.path("data.json")}, wantStdoutHas: []string{"id  user.name", "1   Ada"}},
		{name: "short aliases", flags: []string{"I", "J", "E"}, args: []string{"-J", "-I", "-E", "utf-8", "--sample", "1", fx.path("data.json")}, wantStdoutHas: []string{`"kind":"json"`}},
		{name: "compact aliases", flags: []string{"P", "G"}, args: []string{"-F", "-G", "TODO", fx.path("a.txt")}, wantStdoutHas: []string{"<ROOT>/a.txt:1:7:"}},
		{name: "missing pattern file", flags: []string{"f"}, args: []string{"-f", fx.path("missing.patterns"), fx.root}, wantExit: 2, wantStderrHas: []string{"no such file"}},
		{name: "invalid regex", flags: []string{}, args: []string{"[", fx.path("a.txt")}, wantExit: 2, wantStderrHas: []string{"invalid pattern"}},
		{name: "stdin", flags: []string{"-"}, args: []string{"-F", "TODO", "-"}, stdin: "TODO via stdin\nnope\n", wantStdout: "-:1:1-4\n    TODO via stdin\n"},
		{name: "cpuprofile", flags: []string{"cpuprofile"}, args: []string{"-F", "-c", "--cpuprofile", fx.path("cpu.out"), "TODO", fx.path("a.txt")}, wantStdout: "2\n"},
		{name: "stats", flags: []string{"stats", "s"}, args: []string{"-F", "-s", "-c", "TODO", fx.path("a.txt")}, wantStdout: "2\n", wantStderrHas: []string{"zrep: files=1"}},
	}

	covered := map[string]bool{}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			run := runZrepCommand(t, fx, tc)
			if run.code != tc.wantExit {
				t.Fatalf("exit code: got %d want %d\nstdout:\n%s\nstderr:\n%s", run.code, tc.wantExit, run.stdout, run.stderr)
			}
			if tc.normalizeNulls {
				run.stdout = strings.ReplaceAll(run.stdout, "\x00", "<NUL>")
			}
			if tc.wantStdout != "" {
				assertGolden(t, tc.name+" stdout", run.stdout, tc.wantStdout)
			}
			if tc.wantStderr != "" {
				assertGolden(t, tc.name+" stderr", run.stderr, tc.wantStderr)
			}
			for _, want := range tc.wantStdoutHas {
				if !strings.Contains(run.stdout, want) {
					t.Fatalf("stdout missing %q\nstdout:\n%s", want, run.stdout)
				}
			}
			for _, want := range tc.wantStderrHas {
				if !strings.Contains(run.stderr, want) {
					t.Fatalf("stderr missing %q\nstderr:\n%s", want, run.stderr)
				}
			}
		})
		for _, name := range tc.flags {
			covered[name] = true
		}
	}
	for _, name := range requiredTestedFlags() {
		if !covered[name] {
			t.Fatalf("flag %q is not covered by TestFlagExamples", name)
		}
	}
}

func TestREADMEFlagCoverage(t *testing.T) {
	ensureAllFlagsRegisteredForTest()
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, name := range registeredFlagNames() {
		marker := "<!-- flag:" + name + " -->"
		if !strings.Contains(readme, marker) {
			t.Fatalf("README missing flag marker %s", marker)
		}
	}
	for _, name := range []string{"ZREP_CONFIG_PATH", "ZREP_NO_CONFIG", "ZREP_PROFILE", "-", "list-files", "without-match", "matches-count", "all-lines", "cell-width"} {
		marker := "<!-- flag:" + name + " -->"
		if !strings.Contains(readme, marker) {
			t.Fatalf("README missing feature marker %s", marker)
		}
	}
}

func newFlagFixture(t *testing.T) flagFixture {
	t.Helper()
	root := t.TempDir()
	fx := flagFixture{root: root, files: map[string]string{}}
	write := func(rel, data string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		fx.files[rel] = path
	}
	write("a.txt", "alpha TODO one\nbeta two\nalpha TODO TODO three\nFIXME item\n")
	write("b.log", "nothing here\n")
	write("sub/c.log", "TODO in log\n")
	write(".hidden/secret.txt", "TODO hidden\n")
	write("ignored.ignoreme", "TODO ignored\n")
	write("node_modules/pkg.txt", "TODO dependency\n")
	write("words.txt", "TODO todoish\n")
	write("lines.txt", "TODO\nnot TODO\n")
	write("space.txt", "    TODO indented\n")
	write("long.txt", "prefix TODO xxxxxxxxxxxxxxxxxxxxxxxxx\n")
	write("multi.txt", "start\nmiddle TODO\nend\n")
	write("custom.mine", "TODO custom\n")
	write("data.csv", "id,login,note\n1,ada,TODO csv\n2,bob,plain\n")
	write("data.json", `[{"id":1,"login":"ada","user":{"name":"Ada"},"note":"TODO json"},{"id":2,"login":"bob","user":{"name":"Bob"},"note":"plain"}]`+"\n")
	write("data.jsonl", `{"id":1,"login":"ada","note":"TODO jsonl"}`+"\n")
	write("advanced.txt", "user=42 action=login\nauthorization failed\nfatal timeout happened\n")
	write("logs.txt", "2026-05-01T10:00:00Z ERROR service=api failed\n2026-05-01T10:30:00Z ERROR service=worker timeout\n2026-05-01T11:00:00Z INFO service=api ok\n")
	write(".gitignore", "*.ignoreme\n")
	write("custom.ignore", "a.txt\n")
	write("real-link-dir/linked.txt", "LINKTODO through symlink\n")
	if err := os.Symlink(filepath.Join(root, "real-link-dir"), filepath.Join(root, "link-dir")); err == nil {
		fx.files["link-dir"] = filepath.Join(root, "link-dir")
	}
	fx.pattern = filepath.Join(root, "patterns.txt")
	write("patterns.txt", "TODO\nFIXME\n")
	fx.config = filepath.Join(root, "zrep.flags")
	write("zrep.flags", "-F\n--count-matches\n--include 'a.txt'\n")
	fx.bclConfig = filepath.Join(root, "zrep.bcl")
	write("zrep.bcl", `global ["-F", "--sort", "path"]
profiles {
  code ["-F", "--glob", "*.log", "--sort", "path"]
  logs ["-F", "--passthru", "--max-count", "1"]
}
`)
	fx.xdg = filepath.Join(root, "xdg")
	write("xdg/zrep/config.bcl", `global ["-F", "--sort", "path"]
profiles {
  code ["-F", "--glob", "*.log", "--sort", "path"]
  logs ["-F", "--passthru", "--max-count", "1"]
}
`)
	writeBinary(t, filepath.Join(root, "binary.bin"), []byte("prefix\x00TODO binary\n"))
	writeUTF16LE(t, filepath.Join(root, "utf16.txt"), "TODO utf16\n")
	writeGzip(t, filepath.Join(root, "archive.gz"), "GZTODO\n")
	writeZip(t, filepath.Join(root, "archive.zip"), "inside.txt", "ZIPTODO\n")
	return fx
}

func (f flagFixture) path(rel string) string {
	if path, ok := f.files[rel]; ok {
		return path
	}
	return filepath.Join(f.root, filepath.FromSlash(rel))
}

func (f flagFixture) copyPath(t *testing.T, rel, data string) string {
	t.Helper()
	path := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runZrepCommand(t *testing.T, fx flagFixture, tc flagExampleCase) zrepRun {
	t.Helper()
	cmd := exec.Command(zrepBinary(t), tc.args...)
	cmd.Env = testEnv(tc.env...)
	if tc.stdin != "" {
		cmd.Stdin = strings.NewReader(tc.stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %s failed: %v", tc.name, err)
		}
	}
	return zrepRun{
		stdout: normalizeOutput(fx, stdout.String()),
		stderr: normalizeOutput(fx, stderr.String()),
		code:   code,
	}
}

func normalizeOutput(fx flagFixture, s string) string {
	pipeRoot := strings.ReplaceAll(filepath.ToSlash(fx.root), "/", "|")
	s = strings.ReplaceAll(s, pipeRoot, "<ROOT>")
	s = strings.ReplaceAll(s, filepath.ToSlash(fx.root), "<ROOT>")
	s = strings.ReplaceAll(s, fx.root, "<ROOT>")
	return filepath.ToSlash(s)
}

func zrepBinary(t *testing.T) string {
	t.Helper()
	buildZrepOnce.Do(func() {
		dir, err := os.MkdirTemp("", "zrep-test-bin-*")
		if err != nil {
			builtZrepErr = err
			return
		}
		builtZrepPath = filepath.Join(dir, "zrep")
		cmd := exec.Command("go", "build", "-o", builtZrepPath, ".")
		out, err := cmd.CombinedOutput()
		if err != nil {
			builtZrepErr = fmt.Errorf("build zrep: %w\n%s", err, out)
		}
	})
	if builtZrepErr != nil {
		t.Fatal(builtZrepErr)
	}
	return builtZrepPath
}

func assertGolden(t *testing.T, name, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s mismatch\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func writeBinary(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeUTF16LE(t *testing.T, path, text string) {
	t.Helper()
	u16 := utf16.Encode([]rune(text))
	out := make([]byte, 0, len(u16)*2)
	for _, r := range u16 {
		out = append(out, byte(r), byte(r>>8))
	}
	writeBinary(t, path, out)
}

func writeGzip(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	if _, err := gw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeZip(t *testing.T, path, name, text string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func ensureAllFlagsRegisteredForTest() {
	registerFlagsOnce.Do(func() {
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
		flag.Var(&flagTypes, "t", "include files of type; may be repeated")
		flag.Var(&flagTypeExcludes, "T", "exclude files of type; may be repeated")
		flag.Var(&flagTypeAdds, "type-add", "add file type as name:glob; may be repeated")
		registerShortAliases()
	})
}

func registeredFlagNames() []string {
	ensureAllFlagsRegisteredForTest()
	names := make([]string, 0)
	flag.VisitAll(func(f *flag.Flag) {
		if strings.HasPrefix(f.Name, "test.") {
			return
		}
		names = append(names, f.Name)
	})
	sort.Strings(names)
	return names
}

func requiredTestedFlags() []string {
	names := append(registeredFlagNames(), "ZREP_CONFIG_PATH", "ZREP_NO_CONFIG", "ZREP_PROFILE", "-")
	sort.Strings(names)
	return names
}
