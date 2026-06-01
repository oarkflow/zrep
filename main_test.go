package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zrep/zrep/internal/testdatafixture"
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
	config := filepath.Join(dir, "zrep.flags")
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

func TestBCLConfigProfiles(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zrep.bcl")
	mustWrite(t, config, `global ["-F", "--sort", "path"]
profiles {
  code ["--glob", "*.go", "--sort", "path"]
  logs ["--passthru", "--max-count", "1"]
}
`)
	mustWrite(t, filepath.Join(dir, "a.txt"), "TODO TODO\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "TODO go\n")

	out := runZrepEnv(t, []string{"ZREP_NO_CONFIG=0"}, "--config", config, "TODO", dir)
	if !strings.Contains(out, "a.txt") || !strings.Contains(out, "main.go") {
		t.Fatalf("global BCL config output = %q", out)
	}

	out = runZrepEnv(t, []string{"ZREP_NO_CONFIG=0"}, "--config", config, "--profile", "code", "TODO", dir)
	if !strings.Contains(out, "main.go") || strings.Contains(out, "a.txt") {
		t.Fatalf("profile BCL config output = %q", out)
	}

	out = runZrepEnv(t, []string{"ZREP_NO_CONFIG=0"}, "--config", config, "--config-id", "logs", "TODO", filepath.Join(dir, "a.txt"))
	if !strings.Contains(out, "TODO TODO") {
		t.Fatalf("config-id BCL config output = %q", out)
	}
}

func TestBCLConfigEnvProfileAndMissingProfile(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zrep.bcl")
	mustWrite(t, config, `global ["-F"]
profiles {
  code ["--glob", "*.go"]
}
`)
	mustWrite(t, filepath.Join(dir, "a.txt"), "TODO txt\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "TODO go\n")

	out := runZrepEnv(t, []string{"ZREP_CONFIG_PATH=" + config, "ZREP_PROFILE=code", "ZREP_NO_CONFIG=0"}, "TODO", dir)
	if !strings.Contains(out, "main.go") || strings.Contains(out, "a.txt") {
		t.Fatalf("env profile output = %q", out)
	}

	cmd := exec.Command("go", "run", ".", "--config", config, "--profile", "missing", "TODO", dir)
	cmd.Env = testEnv("ZREP_NO_CONFIG=0")
	outBytes, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected missing profile to fail, output:\n%s", outBytes)
	}
	if !strings.Contains(string(outBytes), `config profile "missing" not found`) {
		t.Fatalf("missing profile output = %s", outBytes)
	}
}

func TestBCLProfileUsesDefaultConfigWithoutConfigFlag(t *testing.T) {
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	configDir := filepath.Join(xdg, "zrep")
	config := filepath.Join(configDir, "config.bcl")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, config, `global ["-F", "--sort", "path"]
profiles {
  code ["--glob", "*.go"]
  logs ["--passthru", "--max-count", "1"]
}
`)
	mustWrite(t, filepath.Join(dir, "a.txt"), "TODO txt\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "TODO go\n")

	out := runZrepEnv(t, []string{"XDG_CONFIG_HOME=" + xdg, "ZREP_NO_CONFIG=0"}, "--profile", "code", "TODO", dir)
	if !strings.Contains(out, "main.go") || strings.Contains(out, "a.txt") {
		t.Fatalf("default profile output = %q", out)
	}

	out = runZrepEnv(t, []string{"XDG_CONFIG_HOME=" + xdg, "ZREP_NO_CONFIG=0"}, "--config-id", "logs", "TODO", filepath.Join(dir, "a.txt"))
	if !strings.Contains(out, "TODO txt") {
		t.Fatalf("default config-id output = %q", out)
	}
}

func TestBCLDefaultConfigEnvProfileAndMissingDefault(t *testing.T) {
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	configDir := filepath.Join(xdg, "zrep")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(configDir, "config.bcl"), `global ["-F"]
profiles {
  code ["--glob", "*.go"]
}
`)
	mustWrite(t, filepath.Join(dir, "a.txt"), "TODO txt\n")
	mustWrite(t, filepath.Join(dir, "main.go"), "TODO go\n")

	out := runZrepEnv(t, []string{"XDG_CONFIG_HOME=" + xdg, "ZREP_PROFILE=code", "ZREP_NO_CONFIG=0"}, "TODO", dir)
	if !strings.Contains(out, "main.go") || strings.Contains(out, "a.txt") {
		t.Fatalf("default env profile output = %q", out)
	}

	missingXDG := filepath.Join(dir, "missing-xdg")
	cmd := exec.Command("go", "run", ".", "--profile", "code", "TODO", dir)
	cmd.Env = testEnv("XDG_CONFIG_HOME="+missingXDG, "ZREP_NO_CONFIG=0")
	outBytes, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected missing default config to fail, output:\n%s", outBytes)
	}
	if !strings.Contains(string(outBytes), `config profile "code" requested but no default config was found`) {
		t.Fatalf("missing default config output = %s", outBytes)
	}
}

func TestBCLProfileManagementCommands(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.bcl")

	out := runZrepEnv(t, []string{"ZREP_NO_CONFIG=0"}, "--config", config, "profile", "import", "code", "--", "-F", "--glob", "*.go")
	if !strings.Contains(out, `imported profile "code"`) {
		t.Fatalf("import output = %q", out)
	}

	out = runZrepEnv(t, []string{"ZREP_NO_CONFIG=0"}, "--config", config, "profile", "list")
	if strings.TrimSpace(out) != "code" {
		t.Fatalf("list output = %q", out)
	}

	out = runZrepEnv(t, []string{"ZREP_NO_CONFIG=0"}, "--config", config, "profile", "update", "code", "--", "-F", "--glob", "*.ts")
	if !strings.Contains(out, `updated profile "code"`) {
		t.Fatalf("update output = %q", out)
	}
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"*.ts"`) || strings.Contains(string(b), `"*.go"`) {
		t.Fatalf("updated config = %s", b)
	}

	out = runZrepEnv(t, []string{"ZREP_NO_CONFIG=0"}, "--config", config, "profile", "remove", "code")
	if !strings.Contains(out, `removed profile "code"`) {
		t.Fatalf("remove output = %q", out)
	}
}

func TestBCLProfileManagementUsesDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	xdg := filepath.Join(dir, "xdg")
	out := runZrepEnv(t, []string{"XDG_CONFIG_HOME=" + xdg, "ZREP_NO_CONFIG=0"}, "profile", "import", "logs", "--", "-F", "--passthru")
	if !strings.Contains(out, `imported profile "logs"`) {
		t.Fatalf("default import output = %q", out)
	}
	config := filepath.Join(xdg, "zrep", "config.bcl")
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "logs") || !strings.Contains(string(b), "--passthru") {
		t.Fatalf("default config = %s", b)
	}
}

func TestNoConfigDisablesConfigExpansion(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "zrep.flags")
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

func TestGeneratedTestdataWorkflows(t *testing.T) {
	fx := testdatafixture.Generated(t, testdatafixture.Options{Rows: 48, Files: 2, Days: 2, MatchEvery: 6})

	out := runZrep(t, "--inspect", "--select", "id,email", "--format", "table", "--limit", "2", fx.Structured.UsersJSON)
	if !strings.Contains(out, "ada@example.com") || !strings.Contains(out, "email") {
		t.Fatalf("large JSON inspect workflow output wrong:\n%s", out)
	}

	out = runZrep(t, "--profile-data", fx.Structured.UsersCSV)
	if !strings.Contains(out, "rows: 48") || !strings.Contains(out, "email nulls=0") {
		t.Fatalf("large CSV profile workflow output wrong:\n%s", out)
	}

	out = runZrep(t, "--logs", "--user-id", "42", "--activity", "login", "--from", "2026-05-01", "--to", "2026-05-03", fx.Logs.Root)
	if !strings.Contains(out, "user_id=42") || !strings.Contains(out, "activity=login") {
		t.Fatalf("daily log factor workflow output wrong:\n%s", out)
	}

	out = runZrep(t, "--logs", "--xql", `zrep_records | where status == 500 | select __zrep_index`, fx.Logs.Activity)
	if !strings.Contains(out, "payment failed") {
		t.Fatalf("xql log workflow output wrong:\n%s", out)
	}

	out = runZrep(t, "--sql-files", "--sql-block", "object", "--sql-kind", "create", "users", fx.SQL.Root)
	if !strings.Contains(out, "CREATE TABLE users") {
		t.Fatalf("SQL block workflow output wrong:\n%s", out)
	}

	out = runZrep(t, "-F", "--search-compressed", "ZREP_NEEDLE", fx.Archives.Gzip)
	if !strings.Contains(out, "gzip") {
		t.Fatalf("compressed workflow output wrong:\n%s", out)
	}

	out = runZrep(t, "-F", "--text", "ZREP_NEEDLE", fx.Search.Binary)
	if !strings.Contains(out, "binary") {
		t.Fatalf("binary-as-text workflow output wrong:\n%s", out)
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
