package testdatafixture

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var stressRoot = flag.String("stress-root", "", "write an optional zrep stress fixture corpus to this directory")

func TestGeneratedFixtureCoversExpectedCategories(t *testing.T) {
	fx := Generated(t, Options{Rows: 24, Files: 2, Days: 2, MatchEvery: 4})
	for name, path := range map[string]string{
		"users json":    fx.Structured.UsersJSON,
		"users csv":     fx.Structured.UsersCSV,
		"daily log":     fx.Logs.DayText[0],
		"daily jsonl":   fx.Logs.DayJSONL[0],
		"activity log":  fx.Logs.Activity,
		"sql migration": fx.SQL.Migration,
		"search text":   fx.Search.Text,
		"binary":        fx.Search.Binary,
		"utf16":         fx.Search.UTF16,
		"gzip":          fx.Archives.Gzip,
		"zip":           fx.Archives.Zip,
		"bcl":           fx.Config.BCL,
		"patterns":      fx.Config.Patterns,
		"manifest":      filepath.Join(fx.Root, "MANIFEST.csv"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s missing at %s: %v", name, path, err)
		}
	}
	if len(fx.Manifest) < 20 {
		t.Fatalf("expected broad manifest coverage, got %d entries", len(fx.Manifest))
	}
	b, err := os.ReadFile(fx.Logs.Activity)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "user_id=42") || !strings.Contains(string(b), Marker) {
		t.Fatalf("activity fixture missing expected factors:\n%s", b)
	}
}

func TestSmallFixturePathsPointAtCheckedInTree(t *testing.T) {
	fx := Small()
	for _, path := range []string{fx.Structured.UsersJSON, fx.Logs.Activity, fx.SQL.Migration, fx.Config.BCL} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("small fixture path missing: %s: %v", path, err)
		}
	}
}

func TestGenerateStressCorpus(t *testing.T) {
	if *stressRoot == "" && os.Getenv("ZREP_STRESS_TESTDATA") == "" {
		t.Skip("set -stress-root or ZREP_STRESS_TESTDATA=1 to generate stress fixtures")
	}
	root := *stressRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "zrep-testdata")
	}
	fx, err := Stress(root, Options{
		Rows:       envInt("ZREP_STRESS_ROWS", 10000),
		Files:      envInt("ZREP_STRESS_FILES", 16),
		Days:       envInt("ZREP_STRESS_DAYS", 14),
		MatchEvery: envInt("ZREP_STRESS_MATCH_EVERY", 97),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", FormatManifest(fx))
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil || n <= 0 {
		return fallback
	}
	return n
}
