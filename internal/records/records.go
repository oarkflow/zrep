package records

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zrep/zrep/internal/query"
)

type Record struct {
	Path      string
	LineStart int
	LineEnd   int
	Column    int
	Text      string
	Fields    query.Record
}

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	ipRe    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

func ReadLogRecords(path string) ([]Record, error) {
	r, closeFn, err := openText(path)
	if err != nil {
		return nil, err
	}
	defer closeFn()

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var out []Record
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := sc.Text()
		fields := ExtractLogFields(line)
		fields["path"] = path
		fields["line"] = intString(lineNum)
		out = append(out, Record{Path: path, LineStart: lineNum, LineEnd: lineNum, Column: 1, Text: line, Fields: fields})
	}
	return out, sc.Err()
}

func ExtractLogFields(line string) query.Record {
	fields := query.Record{"text": line, "message": line}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(trimmed), &obj) == nil {
			flattenJSON(fields, "", obj)
		}
	}
	for _, token := range splitLogTokens(line) {
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			key, value, ok = strings.Cut(token, ":")
		}
		if !ok {
			continue
		}
		key = normalizeKey(key)
		value = strings.Trim(strings.TrimSpace(value), `"'[],`)
		if key != "" && value != "" {
			fields[key] = value
		}
	}
	if _, ok := fields["email"]; !ok {
		if email := emailRe.FindString(line); email != "" {
			fields["email"] = email
		}
	}
	if _, ok := fields["ip"]; !ok {
		if ip := ipRe.FindString(line); ip != "" {
			fields["ip"] = ip
		}
	}
	if _, ok := fields["level"]; !ok {
		for _, token := range strings.Fields(line) {
			level := strings.ToUpper(strings.Trim(token, "[]:"))
			switch level {
			case "TRACE", "DEBUG", "INFO", "WARN", "WARNING", "ERROR", "FATAL":
				fields["level"] = level
			}
		}
	}
	if _, ok := fields["timestamp"]; !ok {
		if ts, ok := ParseLogTime(line); ok {
			fields["timestamp"] = ts.Format(time.RFC3339)
		}
	}
	if v := firstNonEmpty(fields, "user", "uid", "userid"); v != "" {
		fields["user_id"] = v
	}
	if v := firstNonEmpty(fields, "action", "event", "activity_name"); v != "" {
		fields["activity"] = v
	}
	return fields
}

func ParseLogTime(line string) (time.Time, bool) {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return time.Time{}, false
	}
	candidates := []string{strings.Trim(parts[0], "[]")}
	if len(parts) > 1 {
		candidates = append(candidates, strings.Trim(parts[0]+" "+parts[1], "[]"))
	}
	for _, candidate := range candidates {
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
			if t, err := time.ParseInLocation(layout, candidate, time.Local); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

type SQLBlock struct {
	Record
	Kind   string
	Object string
	Table  string
}

func ReadSQLBlocks(path string, mode string, contextLines int) ([]SQLBlock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	statements := splitSQLStatements(string(b))
	lines := strings.Split(string(b), "\n")
	var out []SQLBlock
	for _, st := range statements {
		text := strings.TrimSpace(st.text)
		if text == "" {
			continue
		}
		kind, object, table := sqlMetadata(text)
		blockText := text
		lineStart, lineEnd := st.startLine, st.endLine
		if mode == "context" {
			start := max(1, st.startLine-contextLines)
			end := min(len(lines), st.endLine+contextLines)
			blockText = strings.Join(lines[start-1:end], "\n")
			lineStart, lineEnd = start, end
		}
		fields := query.Record{
			"kind":       kind,
			"object":     object,
			"table":      table,
			"path":       path,
			"line_start": intString(lineStart),
			"line_end":   intString(lineEnd),
			"text":       blockText,
		}
		out = append(out, SQLBlock{
			Record: Record{Path: path, LineStart: lineStart, LineEnd: lineEnd, Column: 1, Text: blockText, Fields: fields},
			Kind:   kind,
			Object: object,
			Table:  table,
		})
	}
	return out, nil
}

type sqlStatement struct {
	text      string
	startLine int
	endLine   int
}

func splitSQLStatements(s string) []sqlStatement {
	var out []sqlStatement
	var b strings.Builder
	line := 1
	startLine := 1
	inSingle, inDouble, inLineComment, inBlockComment := false, false, false, false
	dollarTag := ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		next := byte(0)
		if i+1 < len(s) {
			next = s[i+1]
		}
		b.WriteByte(c)
		if c == '\n' {
			line++
			inLineComment = false
		}
		if dollarTag != "" {
			if strings.HasPrefix(s[i:], dollarTag) && i != 0 {
				for j := 1; j < len(dollarTag); j++ {
					if i+j < len(s) {
						b.WriteByte(s[i+j])
					}
				}
				i += len(dollarTag) - 1
				dollarTag = ""
			}
			continue
		}
		if inLineComment {
			continue
		}
		if inBlockComment {
			if c == '*' && next == '/' {
				b.WriteByte(next)
				i++
				inBlockComment = false
			}
			continue
		}
		if inSingle {
			if c == '\'' && next == '\'' {
				b.WriteByte(next)
				i++
				continue
			}
			if c == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if c == '"' {
				inDouble = false
			}
			continue
		}
		if c == '-' && next == '-' {
			b.WriteByte(next)
			i++
			inLineComment = true
			continue
		}
		if c == '/' && next == '*' {
			b.WriteByte(next)
			i++
			inBlockComment = true
			continue
		}
		if c == '\'' {
			inSingle = true
			continue
		}
		if c == '"' {
			inDouble = true
			continue
		}
		if c == '$' {
			if tag, ok := readDollarTag(s[i:]); ok {
				dollarTag = tag
			}
			continue
		}
		if c == ';' {
			out = append(out, sqlStatement{text: b.String(), startLine: startLine, endLine: line})
			b.Reset()
			startLine = line
		}
	}
	if strings.TrimSpace(b.String()) != "" {
		out = append(out, sqlStatement{text: b.String(), startLine: startLine, endLine: line})
	}
	return out
}

func sqlMetadata(text string) (kind, object, table string) {
	text = trimSQLTrivia(text)
	words := sqlWords(text)
	if len(words) == 0 {
		return "", "", ""
	}
	kind = strings.ToLower(words[0])
	switch kind {
	case "create":
		if len(words) > 2 {
			object = strings.ToLower(words[1])
			table = cleanSQLIdent(words[2])
			if object != "table" && object != "index" && len(words) > 3 {
				table = cleanSQLIdent(words[3])
			}
		}
	case "insert":
		table = wordAfter(words, "into")
	case "update":
		if len(words) > 1 {
			table = cleanSQLIdent(words[1])
		}
	case "delete":
		table = wordAfter(words, "from")
	case "alter", "drop":
		if len(words) > 2 {
			object = strings.ToLower(words[1])
			table = cleanSQLIdent(words[2])
		}
	case "select":
		table = wordAfter(words, "from")
	}
	return kind, object, table
}

func trimSQLTrivia(text string) string {
	text = strings.TrimSpace(text)
	for {
		switch {
		case strings.HasPrefix(text, "--"):
			if idx := strings.IndexByte(text, '\n'); idx >= 0 {
				text = strings.TrimSpace(text[idx+1:])
				continue
			}
			return ""
		case strings.HasPrefix(text, "/*"):
			if idx := strings.Index(text, "*/"); idx >= 0 {
				text = strings.TrimSpace(text[idx+2:])
				continue
			}
			return ""
		default:
			return text
		}
	}
}

func SelectedFields(records []Record) []string {
	seen := map[string]bool{}
	var fields []string
	for _, rec := range records {
		for key := range rec.Fields {
			if !seen[key] {
				seen[key] = true
				fields = append(fields, key)
			}
		}
	}
	sort.Strings(fields)
	return fields
}

func openText(path string) (io.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".gz", ".tgz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, func() {}, err
		}
		return gr, func() { gr.Close(); f.Close() }, nil
	case ".bz2":
		return bzip2.NewReader(f), func() { f.Close() }, nil
	default:
		return f, func() { f.Close() }, nil
	}
}

func flattenJSON(out query.Record, prefix string, obj map[string]any) {
	for key, value := range obj {
		name := normalizeKey(key)
		if prefix != "" {
			name = prefix + "." + name
		}
		if child, ok := value.(map[string]any); ok {
			flattenJSON(out, name, child)
			continue
		}
		out[name] = stringify(value)
	}
}

func splitLogTokens(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ',' || r == ';'
	})
}

func normalizeKey(key string) string {
	key = strings.Trim(strings.TrimSpace(key), `"'[]{}(),`)
	key = strings.ReplaceAll(key, "-", "_")
	return strings.ToLower(key)
}

func firstNonEmpty(fields query.Record, keys ...string) string {
	for _, key := range keys {
		if value := fields[key]; value != "" {
			return value
		}
	}
	return ""
}

func readDollarTag(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	end := strings.IndexByte(s[1:], '$')
	if end < 0 {
		return "", false
	}
	tag := s[:end+2]
	for _, r := range tag[1 : len(tag)-1] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return "", false
		}
	}
	return tag, true
}

func sqlWords(text string) []string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "(")
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '\t' || r == '(' || r == ')' || r == ',' || r == ';'
	})
}

func wordAfter(words []string, needle string) string {
	for i := 0; i+1 < len(words); i++ {
		if strings.EqualFold(words[i], needle) {
			return cleanSQLIdent(words[i+1])
		}
	}
	return ""
}

func cleanSQLIdent(value string) string {
	return strings.ToLower(strings.Trim(value, "\"'`;"))
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(bytes.Trim(b, `"`))
	}
}

func intString(n int) string {
	if n == 0 {
		return ""
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
