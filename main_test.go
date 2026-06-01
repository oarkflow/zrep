package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatchGlobPlatformAgnostic(t *testing.T) {
	tests := []struct {
		pattern string
		value   string
	}{
		{"*.js", "APP.JS"},
		{"*.json", "Config.JSON"},
		{"src/*.go", "src/main.go"},
		{"src/*.go", `src\main.go`},
		{"Dockerfile.*", "Dockerfile.prod"},
	}

	for _, tt := range tests {
		if !matchGlob(tt.pattern, tt.value) {
			t.Fatalf("expected %q to match %q", tt.pattern, tt.value)
		}
	}
}

func TestTypeCatalogCommonDataAndPlatformTypes(t *testing.T) {
	types := typeCatalog()
	for _, name := range []string{"csv", "js", "json", "windows", "web", "log", "zip"} {
		if len(types[name]) == 0 {
			t.Fatalf("expected type %q to be registered", name)
		}
	}
}

func TestExpandShortFlagClusters(t *testing.T) {
	got := expandShortFlagClusters([]string{"zrep", "-jisrc", "TODO", "."})
	want := []string{"zrep", "-J", "-i", "-s", "-R", "-c", "TODO", "."}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestDoesNotExpandValueFlags(t *testing.T) {
	for _, args := range [][]string{
		{"zrep", "-j8", "TODO"},
		{"zrep", "-N5", "file.csv"},
		{"zrep", "-rDONE", "TODO"},
	} {
		got := expandShortFlagClusters(args)
		if len(got) != len(args) || got[1] != args[1] {
			t.Fatalf("expected %v to remain unchanged, got %v", args, got)
		}
	}
}

func TestReorderFlagsBeforePositionals(t *testing.T) {
	got := reorderFlagsBeforePositionals([]string{"zrep", "-json", "-inspect", "-sample", "3", "file.json", "--format", "table"})
	want := []string{"zrep", "-json", "-inspect", "-sample", "3", "--format", "table", "file.json"}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestReorderNewValueFlagsBeforePositionals(t *testing.T) {
	got := reorderFlagsBeforePositionals([]string{"zrep", "TODO", "file.txt", "--glob", "*.txt", "--max-count", "1", "--cell-width", "30"})
	want := []string{"zrep", "--glob", "*.txt", "--max-count", "1", "--cell-width", "30", "TODO", "file.txt"}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestConfigExpansion(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zrep.conf")
	mustWrite(t, config, `
# zrep config supports shell-like quotes and comments.
-F
--glob '*.txt'
--sort path
--count-matches
`)
	mustWrite(t, filepath.Join(dir, "a.txt"), "TODO TODO\n")
	mustWrite(t, filepath.Join(dir, "b.log"), "TODO\n")

	out := runZrepEnv(t, []string{"ZREP_CONFIG_PATH=" + config, "ZREP_NO_CONFIG=0"}, "TODO", dir)
	if strings.TrimSpace(out) != filepath.Join(dir, "a.txt")+":2" {
		t.Fatalf("config output = %q", out)
	}
}

func TestNoConfigDisablesConfigExpansion(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zrep.conf")
	mustWrite(t, config, "--files\n")
	mustWrite(t, filepath.Join(dir, "a.txt"), "TODO\n")

	out := runZrepEnv(t, []string{"ZREP_CONFIG_PATH=" + config, "ZREP_NO_CONFIG=0"}, "--no-config", "-F", "TODO", filepath.Join(dir, "a.txt"))
	if !strings.Contains(out, "TODO") {
		t.Fatalf("--no-config did not ignore config:\n%s", out)
	}
}

func TestSplitConfigLine(t *testing.T) {
	got, err := splitConfigLine(`--glob '*.go' --replace "hello world" # comment`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--glob", "*.go", "--replace", "hello world"}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg %d: got %q want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func TestZrepFeatureSmoke(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "alpha TODO one\nbeta two\nalpha TODO TODO three\n")
	mustWrite(t, filepath.Join(dir, "b.log"), "nothing here\n")
	mustWrite(t, filepath.Join(dir, "patterns.txt"), "TODO\nthree\n")
	mustWrite(t, filepath.Join(dir, "data.json"), `[{"id":1,"login":"a","user":{"name":"Ada"}},{"id":2,"login":"b","user":{"name":"Bob"}}]`)

	out := runZrep(t, "--files", dir, "--sort", "path")
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "b.log") {
		t.Fatalf("--files output missing files:\n%s", out)
	}

	out = runZrep(t, "-F", "-f", filepath.Join(dir, "patterns.txt"), "-c", dir, "--include-zero", "--sort", "path")
	if !strings.Contains(out, "a.txt:2") || !strings.Contains(out, "b.log:0") {
		t.Fatalf("pattern file count output wrong:\n%s", out)
	}

	out = runZrep(t, "-F", "--files-without-match", "TODO", dir, "--sort", "path")
	if !strings.Contains(out, "b.log") || strings.Contains(out, "a.txt") {
		t.Fatalf("files-without-match output wrong:\n%s", out)
	}

	out = runZrep(t, "-F", "--count-matches", "TODO", filepath.Join(dir, "a.txt"))
	if strings.TrimSpace(out) != "3" {
		t.Fatalf("count-matches output = %q", out)
	}

	out = runZrep(t, "-inspect", "--select", "id,user.name", "--flatten", "--where", "login=b", "--format", "table", "--limit", "2", filepath.Join(dir, "data.json"))
	if !strings.Contains(out, "user.name") || !strings.Contains(out, "Bob") || strings.Contains(out, "Ada") {
		t.Fatalf("inspect select/where/flatten output wrong:\n%s", out)
	}
}

func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runZrep(t *testing.T, args ...string) string {
	return runZrepEnv(t, nil, args...)
}

func runZrepEnv(t *testing.T, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "."}, args...)...)
	cmd.Env = testEnv(extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run . %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func testEnv(extra ...string) []string {
	env := append([]string(nil), os.Environ()...)
	env = setEnv(env, "ZREP_TEST_CHILD=1")
	env = setEnv(env, "ZREP_NO_CONFIG=1")
	for _, item := range extra {
		env = setEnv(env, item)
	}
	return env
}

func setEnv(env []string, item string) []string {
	key, _, ok := strings.Cut(item, "=")
	if !ok {
		return append(env, item)
	}
	prefix := key + "="
	for i := range env {
		if strings.HasPrefix(env[i], prefix) {
			env[i] = item
			return env
		}
	}
	return append(env, item)
}
