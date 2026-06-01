package testdatafixture

import (
	"archive/zip"
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

const Marker = "ZREP_NEEDLE"

type TestingT interface {
	Helper()
	TempDir() string
	Fatalf(format string, args ...any)
}

type Options struct {
	Rows        int
	Files       int
	Days        int
	Services    []string
	Users       []User
	MatchEvery  int
	TargetBytes int64
}

type User struct {
	ID    string
	Email string
}

type Fixture struct {
	Root     string
	Manifest []ManifestEntry

	Structured StructuredPaths
	Logs       LogPaths
	SQL        SQLPaths
	Search     SearchPaths
	Archives   ArchivePaths
	Config     ConfigPaths
}

type StructuredPaths struct {
	UsersJSON      string
	UsersJSONL     string
	UsersCSV       string
	UsersTSV       string
	NestedJSON     string
	WideCSV        string
	MalformedJSONL string
	MissingJSON    string
}

type LogPaths struct {
	Root     string
	DayText  []string
	DayJSONL []string
	Activity string
	Access   string
	Mixed    string
}

type SQLPaths struct {
	Root       string
	Migration  string
	Statements string
}

type SearchPaths struct {
	Root    string
	Text    string
	Code    string
	Hidden  string
	Ignored string
	UTF16   string
	Binary  string
	Large   string
	Symlink string
}

type ArchivePaths struct {
	Root string
	Gzip string
	Zip  string
}

type ConfigPaths struct {
	Root     string
	BCL      string
	Patterns string
	Ignore   string
}

type ManifestEntry struct {
	Path  string
	Kind  string
	Rows  int
	Bytes int64
	Notes string
}

func DefaultOptions() Options {
	return Options{
		Rows:       128,
		Files:      8,
		Days:       3,
		Services:   []string{"api", "worker", "billing"},
		Users:      []User{{ID: "42", Email: "ada@example.com"}, {ID: "7", Email: "grace@example.com"}, {ID: "99", Email: "linus@example.com"}},
		MatchEvery: 11,
	}
}

func Small() Fixture {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "testdata"))
	return Fixture{
		Root: root,
		Structured: StructuredPaths{
			UsersJSON:      filepath.Join(root, "structured", "users.json"),
			UsersJSONL:     filepath.Join(root, "structured", "users.jsonl"),
			UsersCSV:       filepath.Join(root, "structured", "users.csv"),
			UsersTSV:       filepath.Join(root, "structured", "users.tsv"),
			NestedJSON:     filepath.Join(root, "structured", "nested.json"),
			WideCSV:        filepath.Join(root, "structured", "wide.csv"),
			MalformedJSONL: filepath.Join(root, "structured", "malformed.jsonl"),
			MissingJSON:    filepath.Join(root, "structured", "missing-null.json"),
		},
		Logs: LogPaths{
			Root:     filepath.Join(root, "logs"),
			Activity: filepath.Join(root, "logs", "activity.log"),
			Access:   filepath.Join(root, "logs", "access.log"),
			Mixed:    filepath.Join(root, "logs", "mixed.log"),
		},
		SQL: SQLPaths{
			Root:       filepath.Join(root, "sql"),
			Migration:  filepath.Join(root, "sql", "migration.sql"),
			Statements: filepath.Join(root, "sql", "statements.sql"),
		},
		Search: SearchPaths{
			Root:    filepath.Join(root, "search"),
			Text:    filepath.Join(root, "search", "notes.txt"),
			Code:    filepath.Join(root, "search", "src", "main.go"),
			Hidden:  filepath.Join(root, "search", ".hidden", "secret.txt"),
			Ignored: filepath.Join(root, "search", "ignored", "skip.log"),
		},
		Config: ConfigPaths{
			Root:     filepath.Join(root, "config"),
			BCL:      filepath.Join(root, "config", "zrep.bcl"),
			Patterns: filepath.Join(root, "config", "patterns.txt"),
			Ignore:   filepath.Join(root, "config", "zrep.ignore"),
		},
	}
}

func Generated(t TestingT, opts Options) Fixture {
	t.Helper()
	root := t.TempDir()
	fx, err := Generate(root, opts)
	if err != nil {
		t.Fatalf("generate testdata fixture: %v", err)
	}
	return fx
}

func Stress(root string, opts Options) (Fixture, error) {
	if root == "" {
		return Fixture{}, fmt.Errorf("stress root is required")
	}
	return Generate(root, opts)
}

func Generate(root string, opts Options) (Fixture, error) {
	opts = normalizeOptions(opts)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Fixture{}, err
	}
	g := generator{root: root, opts: opts}
	if err := g.writeStructured(); err != nil {
		return Fixture{}, err
	}
	if err := g.writeLogs(); err != nil {
		return Fixture{}, err
	}
	if err := g.writeSQL(); err != nil {
		return Fixture{}, err
	}
	if err := g.writeSearch(); err != nil {
		return Fixture{}, err
	}
	if err := g.writeArchives(); err != nil {
		return Fixture{}, err
	}
	if err := g.writeConfig(); err != nil {
		return Fixture{}, err
	}
	if err := g.writeManifest(); err != nil {
		return Fixture{}, err
	}
	return g.fixture(), nil
}

func FormatManifest(fx Fixture) string {
	var b strings.Builder
	fmt.Fprintf(&b, "root: %s\n", fx.Root)
	for _, entry := range fx.Manifest {
		fmt.Fprintf(&b, "%s\t%s\trows=%d\tbytes=%d\t%s\n", filepath.ToSlash(entry.Path), entry.Kind, entry.Rows, entry.Bytes, entry.Notes)
	}
	b.WriteString("\nexamples:\n")
	fmt.Fprintf(&b, "zrep --inspect --sample 5 %s\n", fx.Structured.UsersJSON)
	fmt.Fprintf(&b, "zrep --logs --user-id 42 --activity login %s\n", fx.Logs.Root)
	fmt.Fprintf(&b, "zrep --sql-files --sql-block object --sql-kind create users %s\n", fx.SQL.Root)
	fmt.Fprintf(&b, "zrep -F --search-compressed ERROR %s\n", fx.Archives.Gzip)
	return b.String()
}

type generator struct {
	root     string
	opts     Options
	manifest []ManifestEntry
}

func normalizeOptions(opts Options) Options {
	def := DefaultOptions()
	if opts.Rows <= 0 {
		opts.Rows = def.Rows
	}
	if opts.Files <= 0 {
		opts.Files = def.Files
	}
	if opts.Days <= 0 {
		opts.Days = def.Days
	}
	if len(opts.Services) == 0 {
		opts.Services = def.Services
	}
	if len(opts.Users) == 0 {
		opts.Users = def.Users
	}
	if opts.MatchEvery <= 0 {
		opts.MatchEvery = def.MatchEvery
	}
	return opts
}

func (g *generator) fixture() Fixture {
	root := g.root
	var dayText, dayJSONL []string
	for day := 1; day <= g.opts.Days; day++ {
		date := fmt.Sprintf("2026-05-%02d", day)
		dayText = append(dayText, filepath.Join(root, "logs", date, "app.log"))
		dayJSONL = append(dayJSONL, filepath.Join(root, "logs", date, "app.jsonl"))
	}
	return Fixture{
		Root: root, Manifest: g.manifest,
		Structured: StructuredPaths{
			UsersJSON: filepath.Join(root, "structured", "users.json"), UsersJSONL: filepath.Join(root, "structured", "users.jsonl"),
			UsersCSV: filepath.Join(root, "structured", "users.csv"), UsersTSV: filepath.Join(root, "structured", "users.tsv"),
			NestedJSON: filepath.Join(root, "structured", "nested.json"), WideCSV: filepath.Join(root, "structured", "wide.csv"),
			MalformedJSONL: filepath.Join(root, "structured", "malformed.jsonl"), MissingJSON: filepath.Join(root, "structured", "missing-null.json"),
		},
		Logs:     LogPaths{Root: filepath.Join(root, "logs"), DayText: dayText, DayJSONL: dayJSONL, Activity: filepath.Join(root, "logs", "activity.log"), Access: filepath.Join(root, "logs", "access.log"), Mixed: filepath.Join(root, "logs", "mixed.log")},
		SQL:      SQLPaths{Root: filepath.Join(root, "sql"), Migration: filepath.Join(root, "sql", "migration.sql"), Statements: filepath.Join(root, "sql", "statements.sql")},
		Search:   SearchPaths{Root: filepath.Join(root, "search"), Text: filepath.Join(root, "search", "notes.txt"), Code: filepath.Join(root, "search", "src", "main.go"), Hidden: filepath.Join(root, "search", ".hidden", "secret.txt"), Ignored: filepath.Join(root, "search", "ignored", "skip.log"), UTF16: filepath.Join(root, "search", "utf16.txt"), Binary: filepath.Join(root, "search", "binary.bin"), Large: filepath.Join(root, "search", "large.log"), Symlink: filepath.Join(root, "search", "linked")},
		Archives: ArchivePaths{Root: filepath.Join(root, "archives"), Gzip: filepath.Join(root, "archives", "app.log.gz"), Zip: filepath.Join(root, "archives", "bundle.zip")},
		Config:   ConfigPaths{Root: filepath.Join(root, "config"), BCL: filepath.Join(root, "config", "zrep.bcl"), Patterns: filepath.Join(root, "config", "patterns.txt"), Ignore: filepath.Join(root, "config", "zrep.ignore")},
	}
}

func (g *generator) writeStructured() error {
	var jsonArray strings.Builder
	jsonArray.WriteByte('[')
	jsonl, csvRows, tsvRows := &strings.Builder{}, &strings.Builder{}, &strings.Builder{}
	csvRows.WriteString("id,email,activity,service,status,note\n")
	tsvRows.WriteString("id\temail\tactivity\tservice\tstatus\tnote\n")
	for i := 0; i < g.opts.Rows; i++ {
		u := g.user(i)
		service := g.service(i)
		activity := activity(i)
		status := status(i)
		note := note(i, g.opts.MatchEvery)
		obj := fmt.Sprintf(`{"id":%d,"user_id":"%s","email":"%s","activity":"%s","service":"%s","status":%d,"user":{"name":"User %s"},"note":"%s"}`, i+1, u.ID, u.Email, activity, service, status, u.ID, note)
		if i > 0 {
			jsonArray.WriteByte(',')
		}
		jsonArray.WriteString(obj)
		jsonl.WriteString(obj + "\n")
		fmt.Fprintf(csvRows, "%d,%s,%s,%s,%d,%s\n", i+1, u.Email, activity, service, status, note)
		fmt.Fprintf(tsvRows, "%d\t%s\t%s\t%s\t%d\t%s\n", i+1, u.Email, activity, service, status, note)
	}
	jsonArray.WriteString("]\n")
	if err := g.write("structured/users.json", "json array", g.opts.Rows, jsonArray.String(), "large JSON-style array with nested user fields"); err != nil {
		return err
	}
	if err := g.write("structured/users.jsonl", "jsonl", g.opts.Rows, jsonl.String(), "record-per-line JSON logs/data"); err != nil {
		return err
	}
	if err := g.write("structured/users.csv", "csv", g.opts.Rows, csvRows.String(), "CSV rows with user activity fields"); err != nil {
		return err
	}
	if err := g.write("structured/users.tsv", "tsv", g.opts.Rows, tsvRows.String(), "TSV variant for delimiter detection"); err != nil {
		return err
	}
	if err := g.write("structured/nested.json", "json", 2, `{"org":{"id":"acme","users":[{"id":42,"email":"ada@example.com"},{"id":7,"email":"grace@example.com"}]},"note":"`+Marker+` nested"}`+"\n", "deep object for flatten/select tests"); err != nil {
		return err
	}
	if err := g.write("structured/missing-null.json", "json", 3, `[{"id":1,"email":"ada@example.com","note":null},{"id":2},{"id":3,"email":"","note":"missing fields"}]`+"\n", "missing, empty, and null field coverage"); err != nil {
		return err
	}
	if err := g.write("structured/malformed.jsonl", "jsonl", 3, `{"id":1,"ok":true}`+"\n"+`{"id":2,`+"\n"+`{"id":3,"ok":true,"note":"`+Marker+`"}`+"\n", "malformed middle line for resilient readers"); err != nil {
		return err
	}
	return g.writeWideCSV()
}

func (g *generator) writeWideCSV() error {
	path := filepath.Join(g.root, "structured", "wide.csv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	header := []string{"id", "email"}
	for i := 0; i < 32; i++ {
		header = append(header, fmt.Sprintf("metric_%02d", i))
	}
	header = append(header, "note")
	_ = w.Write(header)
	for i := 0; i < g.opts.Rows; i++ {
		row := []string{fmt.Sprint(i + 1), g.user(i).Email}
		for j := 0; j < 32; j++ {
			row = append(row, fmt.Sprint((i+j)%1000))
		}
		row = append(row, note(i, g.opts.MatchEvery))
		_ = w.Write(row)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return g.add(path, "wide csv", g.opts.Rows, "wide table for truncation/profile tests")
}

func (g *generator) writeLogs() error {
	for day := 1; day <= g.opts.Days; day++ {
		date := fmt.Sprintf("2026-05-%02d", day)
		var text, jsonl strings.Builder
		for i := 0; i < g.opts.Rows; i++ {
			u := g.user(i + day)
			service := g.service(i)
			level := "INFO"
			if i%5 == 0 {
				level = "ERROR"
			}
			msg := note(i, g.opts.MatchEvery)
			fmt.Fprintf(&text, "%sT%02d:%02d:00Z %s service=%s user_id=%s email=%s activity=%s ip=10.0.%d.%d request_id=req-%02d-%04d session_id=sess-%s status=%d %s\n", date, i%24, i%60, level, service, u.ID, u.Email, activity(i), day, i%255, day, i, u.ID, status(i), msg)
			fmt.Fprintf(&jsonl, `{"timestamp":"%sT%02d:%02d:00Z","level":"%s","service":"%s","user_id":"%s","email":"%s","activity":"%s","ip":"10.0.%d.%d","request_id":"req-%02d-%04d","session_id":"sess-%s","status":%d,"message":"%s"}`+"\n", date, i%24, i%60, level, service, u.ID, u.Email, activity(i), day, i%255, day, i, u.ID, status(i), msg)
		}
		if err := g.write(filepath.ToSlash(filepath.Join("logs", date, "app.log")), "daily log", g.opts.Rows, text.String(), "key=value daily application log"); err != nil {
			return err
		}
		if err := g.write(filepath.ToSlash(filepath.Join("logs", date, "app.jsonl")), "daily jsonl log", g.opts.Rows, jsonl.String(), "JSONL daily application log"); err != nil {
			return err
		}
	}
	if err := g.write("logs/activity.log", "activity log", 3, "2026-05-01T10:00:00Z INFO service=api user_id=42 email=ada@example.com activity=login ip=10.0.0.1 request_id=req-login session_id=sess-42 status=200 login ok\n2026-05-01T10:05:00Z INFO service=api user_id=42 email=ada@example.com activity=checkout status=200 checkout ok\n2026-05-01T10:10:00Z ERROR service=billing user_id=7 email=grace@example.com activity=payment status=500 payment failed "+Marker+"\n", "focused user activity cases"); err != nil {
		return err
	}
	if err := g.write("logs/access.log", "access log", 2, `10.0.0.1 - ada@example.com [01/May/2026:10:00:00 +0000] "GET /login HTTP/1.1" 200`+"\n"+`10.0.0.2 - grace@example.com [01/May/2026:10:01:00 +0000] "POST /checkout HTTP/1.1" 500 `+Marker+"\n", "HTTP access-style logs"); err != nil {
		return err
	}
	return g.write("logs/mixed.log", "mixed log", 3, "INFO user_id=42 activity=login service=api\nplain text ERROR ada@example.com 10.0.0.1 "+Marker+"\n{\"level\":\"ERROR\",\"service\":\"worker\",\"user_id\":\"7\",\"activity\":\"job\",\"status\":500}\n", "mixed plain, key=value, and JSON lines")
}

func (g *generator) writeSQL() error {
	migration := `-- migration with semicolons in strings
CREATE TABLE users (
  id INTEGER PRIMARY KEY,
  user_id INTEGER,
  email TEXT,
  note TEXT DEFAULT 'created; with semicolon'
);

ALTER TABLE users ADD COLUMN last_login TEXT;

INSERT INTO users (user_id, email, note) VALUES (42, 'ada@example.com', '` + Marker + ` insert');

UPDATE users SET note = 'checkout; done' WHERE user_id = 42;

DELETE FROM users WHERE email = 'old@example.com';

CREATE FUNCTION audit_user() RETURNS trigger AS $$
BEGIN
  INSERT INTO audit_log(user_id, note) VALUES (NEW.user_id, 'changed; inside dollar quote');
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
`
	if err := g.write("sql/migration.sql", "sql migration", 6, migration, "DDL/DML with comments, strings, and dollar quotes"); err != nil {
		return err
	}
	return g.write("sql/statements.sql", "sql statements", 4, "SELECT user_id, email FROM users WHERE user_id = 42;\nDROP TABLE IF EXISTS old_users;\nCREATE INDEX idx_users_email ON users(email);\nINSERT INTO orders(customer_id, user_id) VALUES (7, 42);\n", "statement kind coverage")
}

func (g *generator) writeSearch() error {
	if err := g.write("search/notes.txt", "text", 4, "alpha "+Marker+" one\nbeta two\nTODO daily note\ncustomer_id=42\n", "plain text search"); err != nil {
		return err
	}
	if err := g.write("search/src/main.go", "go", 3, "package main\n\nfunc main() { println(\""+Marker+"\") }\n", "code type filtering"); err != nil {
		return err
	}
	if err := g.write("search/.hidden/secret.txt", "hidden text", 1, Marker+" hidden\n", "hidden path coverage"); err != nil {
		return err
	}
	if err := g.write("search/ignored/skip.log", "ignored log", 1, Marker+" ignored\n", "ignore-file coverage"); err != nil {
		return err
	}
	if err := g.write("search/large.log", "large text", g.opts.Rows*g.opts.Files, strings.Repeat("large line "+Marker+"\n", g.opts.Rows*g.opts.Files), "streaming large-file path"); err != nil {
		return err
	}
	if err := g.writeBytes("search/binary.bin", "binary", 1, []byte("prefix\x00"+Marker+" binary\n"), "binary marker coverage"); err != nil {
		return err
	}
	if err := g.writeUTF16("search/utf16.txt", "utf16", 1, Marker+" utf16\n", "UTF-16LE encoding coverage"); err != nil {
		return err
	}
	realDir := filepath.Join(g.root, "search", "real-linked")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(realDir, "linked.txt"), []byte(Marker+" symlink\n"), 0o644); err != nil {
		return err
	}
	_ = os.Symlink(realDir, filepath.Join(g.root, "search", "linked"))
	return nil
}

func (g *generator) writeArchives() error {
	if err := os.MkdirAll(filepath.Join(g.root, "archives"), 0o755); err != nil {
		return err
	}
	gzPath := filepath.Join(g.root, "archives", "app.log.gz")
	gzf, err := os.Create(gzPath)
	if err != nil {
		return err
	}
	gw := gzip.NewWriter(gzf)
	_, err = gw.Write([]byte("2026-05-01T10:00:00Z ERROR service=api " + Marker + " gzip\n"))
	if closeErr := gw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gzf.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := g.add(gzPath, "gzip", 1, "compressed log sample"); err != nil {
		return err
	}
	zipPath := filepath.Join(g.root, "archives", "bundle.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(zf)
	w, err := zw.Create("inside/app.log")
	if err != nil {
		zf.Close()
		return err
	}
	_, err = w.Write([]byte("ERROR " + Marker + " zip\n"))
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := zf.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return g.add(zipPath, "zip", 1, "zip archive member sample")
}

func (g *generator) writeConfig() error {
	if err := g.write("config/patterns.txt", "patterns", 2, Marker+"\nERROR\n", "pattern-file input"); err != nil {
		return err
	}
	if err := g.write("config/zrep.ignore", "ignore", 1, "ignored/\n*.tmp\n", "custom ignore file"); err != nil {
		return err
	}
	return g.write("config/zrep.bcl", "bcl config", 3, `global ["-F", "--sort", "path"]
profiles {
  logs ["--logs", "--field", "service=api"]
  large_json ["--inspect", "--format", "table", "--max-cell-width", "80"]
  code ["--glob", "*.go", "--exclude-dir", "vendor"]
}
`, "BCL global/profile config")
}

func (g *generator) writeManifest() error {
	var b strings.Builder
	b.WriteString("path,kind,rows,bytes,notes\n")
	for _, entry := range g.manifest {
		fmt.Fprintf(&b, "%s,%s,%d,%d,%q\n", filepath.ToSlash(entry.Path), entry.Kind, entry.Rows, entry.Bytes, entry.Notes)
	}
	return os.WriteFile(filepath.Join(g.root, "MANIFEST.csv"), []byte(b.String()), 0o644)
}

func (g *generator) write(rel, kind string, rows int, data string, notes string) error {
	return g.writeBytes(rel, kind, rows, []byte(data), notes)
}

func (g *generator) writeBytes(rel, kind string, rows int, data []byte, notes string) error {
	path := filepath.Join(g.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	return g.add(path, kind, rows, notes)
}

func (g *generator) writeUTF16(rel, kind string, rows int, text string, notes string) error {
	encoded := utf16.Encode([]rune(text))
	data := make([]byte, 2, 2+len(encoded)*2)
	data[0], data[1] = 0xff, 0xfe
	for _, r := range encoded {
		data = append(data, byte(r), byte(r>>8))
	}
	return g.writeBytes(rel, kind, rows, data, notes)
}

func (g *generator) add(path, kind string, rows int, notes string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	g.manifest = append(g.manifest, ManifestEntry{Path: path, Kind: kind, Rows: rows, Bytes: st.Size(), Notes: notes})
	return nil
}

func (g *generator) user(i int) User      { return g.opts.Users[i%len(g.opts.Users)] }
func (g *generator) service(i int) string { return g.opts.Services[i%len(g.opts.Services)] }

func activity(i int) string {
	switch i % 5 {
	case 0:
		return "login"
	case 1:
		return "checkout"
	case 2:
		return "search"
	case 3:
		return "download"
	default:
		return "logout"
	}
}

func status(i int) int {
	if i%5 == 0 {
		return 500
	}
	return 200
}

func note(i, matchEvery int) string {
	if i%matchEvery == 0 {
		return Marker
	}
	return fmt.Sprintf("plain-%d", i)
}
