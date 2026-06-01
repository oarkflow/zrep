package records

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractLogFieldsFromMixedText(t *testing.T) {
	fields := ExtractLogFields(`2026-05-01T10:00:00Z INFO service=api user_id=42 email=ada@example.com activity=login ip=10.0.0.1 request_id=r1 ok`)
	for key, want := range map[string]string{
		"timestamp":  "2026-05-01T10:00:00Z",
		"level":      "INFO",
		"service":    "api",
		"user_id":    "42",
		"email":      "ada@example.com",
		"activity":   "login",
		"ip":         "10.0.0.1",
		"request_id": "r1",
	} {
		if got := fields[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestExtractLogFieldsFromJSON(t *testing.T) {
	fields := ExtractLogFields(`{"user":{"id":"42"},"email":"ada@example.com","activity":"checkout","status":200}`)
	if got := fields["user.id"]; got != "42" {
		t.Fatalf("user.id = %q", got)
	}
	if got := fields["activity"]; got != "checkout" {
		t.Fatalf("activity = %q", got)
	}
}

func TestReadSQLBlocksHandlesCommentsStringsAndDollarQuotes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schema.sql")
	sql := `-- ; in comment
CREATE TABLE users (
  id INTEGER,
  note TEXT DEFAULT ';'
);

CREATE FUNCTION f() RETURNS text AS $$
BEGIN
  RETURN ';';
END;
$$ LANGUAGE plpgsql;

INSERT INTO orders (customer_id, user_id) VALUES (7, 42);
`
	if err := os.WriteFile(path, []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks, err := ReadSQLBlocks(path, "statement", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	if blocks[0].Kind != "create" || blocks[0].Table != "users" {
		t.Fatalf("first block metadata = kind:%q table:%q", blocks[0].Kind, blocks[0].Table)
	}
	if blocks[2].Kind != "insert" || blocks[2].Table != "orders" {
		t.Fatalf("insert metadata = kind:%q table:%q", blocks[2].Kind, blocks[2].Table)
	}
}
