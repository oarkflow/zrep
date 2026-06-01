package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/zrep/zrep/internal/walker"
)

type inspectResult struct {
	Path     string   `json:"path"`
	Kind     string   `json:"kind"`
	Rows     int64    `json:"rows,omitempty"`
	DataRows int64    `json:"data_rows,omitempty"`
	Columns  []string `json:"columns,omitempty"`
	Sample   []string `json:"sample,omitempty"`
	Err      string   `json:"error,omitempty"`
}

func runInspect(roots []string) {
	workers := *flagWorkers
	if workers <= 0 {
		workers = 4
	}
	w := walker.New(workers)
	w.Filter = buildPathFilter()
	w.Walk(roots...)

	results := make(chan inspectResult, workers*16)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range w.Results {
				results <- inspectFile(entry.Path)
			}
		}()
	}

	go func() {
		w.Wait()
		wg.Wait()
		close(results)
	}()

	for result := range results {
		printInspectResult(result)
	}
}

func inspectFile(path string) inspectResult {
	kind := inspectKind(path)
	if kind == "text" {
		kind = sniffKind(path)
	}
	switch kind {
	case "csv":
		return inspectCSV(path, kind)
	case "json", "jsonl":
		return inspectJSON(path, kind)
	default:
		return inspectLines(path, kind)
	}
}

func inspectKind(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".csv", ".tsv":
		return "csv"
	case ".json", ".jsonc":
		return "json"
	case ".jsonl", ".ndjson":
		return "jsonl"
	default:
		if ext != "" {
			return strings.TrimPrefix(ext, ".")
		}
		return "text"
	}
}

func sniffKind(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "text"
	}
	defer f.Close()

	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	s := strings.TrimSpace(string(buf[:n]))
	if s == "" {
		return "text"
	}
	if strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		if looksJSONLines(s) {
			return "jsonl"
		}
		return "json"
	}
	firstLine := s
	if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
		firstLine = firstLine[:idx]
	}
	if strings.Count(firstLine, ",") >= 1 {
		return "csv"
	}
	if strings.Count(firstLine, "\t") >= 1 {
		return "csv"
	}
	return "text"
}

func looksJSONLines(s string) bool {
	lines := strings.Split(s, "\n")
	nonEmpty := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmpty++
		if !(strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[")) {
			return false
		}
		if nonEmpty >= 2 {
			return true
		}
	}
	return false
}

func inspectCSV(path, kind string) inspectResult {
	f, err := os.Open(path)
	if err != nil {
		return inspectResult{Path: path, Kind: kind, Err: err.Error()}
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	if strings.EqualFold(filepath.Ext(path), ".tsv") {
		r.Comma = '\t'
	}

	var rows int64
	var columns []string
	var allColumns []string
	var sample []string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return inspectResult{Path: path, Kind: kind, Rows: rows, Columns: columns, Sample: sample, Err: err.Error()}
		}
		rows++
		if rows == 1 {
			allColumns = append([]string(nil), rec...)
			columns = append([]string(nil), rec...)
			if selected := selectedInspectFields(); len(selected) > 0 {
				columns = selected
			}
		}
		if rows > 1 && !csvRecordMatches(allColumns, rec) {
			continue
		}
		if *flagSample > 0 && len(sample) < *flagSample {
			sample = append(sample, strings.Join(selectCSVRecord(allColumns, rec), ","))
		}
	}
	dataRows := rows
	if len(columns) > 0 && dataRows > 0 {
		dataRows--
	}
	return inspectResult{Path: path, Kind: kind, Rows: rows, DataRows: dataRows, Columns: columns, Sample: sample}
}

func inspectJSON(path, kind string) inspectResult {
	f, err := os.Open(path)
	if err != nil {
		return inspectResult{Path: path, Kind: kind, Err: err.Error()}
	}
	defer f.Close()

	if kind == "jsonl" {
		return inspectJSONLines(path, kind, f)
	}

	dec := json.NewDecoder(f)
	tok, err := dec.Token()
	if err != nil {
		return inspectResult{Path: path, Kind: kind, Err: err.Error()}
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return inspectResult{Path: path, Kind: kind, Rows: 1}
	}

	res := inspectResult{Path: path, Kind: kind}
	if delim == '[' {
		for dec.More() {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				res.Err = err.Error()
				return res
			}
			res.Rows++
			if len(res.Columns) == 0 {
				res.Columns = inspectJSONKeys(raw)
			}
			if !jsonRawMatches(raw) {
				continue
			}
			if *flagSample > 0 && len(res.Sample) < *flagSample {
				res.Sample = append(res.Sample, compactSelectedJSON(raw))
			}
			if len(res.Columns) == 0 {
				res.Columns = inspectJSONKeys(raw)
			}
		}
		return res
	}
	if delim == '{' {
		keys := make([]string, 0)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				res.Err = err.Error()
				return res
			}
			key, _ := keyTok.(string)
			keys = append(keys, key)
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				res.Err = err.Error()
				return res
			}
		}
		sort.Strings(keys)
		res.Rows = 1
		res.Columns = keys
		return res
	}
	return res
}

func inspectJSONLines(path, kind string, r io.Reader) inspectResult {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	res := inspectResult{Path: path, Kind: kind}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		res.Rows++
		if len(res.Columns) == 0 {
			res.Columns = inspectJSONKeys([]byte(line))
		}
		if !jsonRawMatches([]byte(line)) {
			continue
		}
		if *flagSample > 0 && len(res.Sample) < *flagSample {
			res.Sample = append(res.Sample, compactSelectedJSON([]byte(line)))
		}
	}
	if err := sc.Err(); err != nil {
		res.Err = err.Error()
	}
	return res
}

func inspectLines(path, kind string) inspectResult {
	f, err := os.Open(path)
	if err != nil {
		return inspectResult{Path: path, Kind: kind, Err: err.Error()}
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	res := inspectResult{Path: path, Kind: kind}
	for sc.Scan() {
		res.Rows++
		if *flagSample > 0 && len(res.Sample) < *flagSample {
			res.Sample = append(res.Sample, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		res.Err = err.Error()
	}
	return res
}

func printInspectResult(result inspectResult) {
	format := strings.ToLower(*flagFormat)
	if *flagJSON && format == "plain" {
		format = "json"
	}
	switch format {
	case "json":
		b, _ := json.Marshal(renderInspectResult(result))
		fmt.Println(string(b))
		return
	case "pretty-json", "prettyjson":
		b, _ := json.MarshalIndent(renderInspectResult(result), "", "  ")
		fmt.Println(string(b))
		return
	case "table":
		printInspectTable(result)
		return
	case "csv":
		printInspectCSV(result)
		return
	}
	fmt.Printf("%s\n", result.Path)
	if result.Err != "" {
		fmt.Printf("    error: %s\n", result.Err)
		return
	}
	if *flagInspect {
		fmt.Printf("    kind: %s\n", result.Kind)
	}
	if *flagRows || *flagInspect {
		fmt.Printf("    rows: %d\n", result.Rows)
		if result.DataRows > 0 {
			fmt.Printf("    data_rows: %d\n", result.DataRows)
		}
	}
	if (*flagColumns || *flagInspect) && len(result.Columns) > 0 {
		fmt.Printf("    columns: %s\n", strings.Join(result.Columns, ", "))
	}
	if *flagSample > 0 {
		fmt.Println("    sample:")
		for _, line := range result.Sample {
			fmt.Printf("        %s\n", line)
		}
	}
}

func renderInspectResult(result inspectResult) any {
	type rendered struct {
		Path     string   `json:"path"`
		Kind     string   `json:"kind"`
		Rows     int64    `json:"rows,omitempty"`
		DataRows int64    `json:"data_rows,omitempty"`
		Columns  []string `json:"columns,omitempty"`
		Sample   any      `json:"sample,omitempty"`
		Err      string   `json:"error,omitempty"`
	}
	return rendered{
		Path:     result.Path,
		Kind:     result.Kind,
		Rows:     result.Rows,
		DataRows: result.DataRows,
		Columns:  result.Columns,
		Sample:   structuredSample(result),
		Err:      result.Err,
	}
}

func structuredSample(result inspectResult) any {
	if len(result.Sample) == 0 {
		return nil
	}
	if result.Kind == "json" || result.Kind == "jsonl" {
		out := make([]any, 0, len(result.Sample))
		for _, sample := range result.Sample {
			var v any
			if err := json.Unmarshal([]byte(sample), &v); err != nil {
				out = append(out, sample)
				continue
			}
			out = append(out, v)
		}
		return out
	}
	if result.Kind == "csv" && len(result.Columns) > 0 {
		out := make([]map[string]string, 0, len(result.Sample))
		for i, sample := range result.Sample {
			if i == 0 && sample == strings.Join(result.Columns, ",") {
				continue
			}
			row := map[string]string{}
			values := splitCSVSample(sample)
			for j, col := range result.Columns {
				if j < len(values) {
					row[col] = values[j]
				}
			}
			out = append(out, row)
		}
		return out
	}
	return result.Sample
}

func printInspectCSV(result inspectResult) {
	w := csv.NewWriter(os.Stdout)
	defer w.Flush()
	_ = w.Write([]string{"path", "kind", "rows", "data_rows", "columns"})
	_ = w.Write([]string{
		result.Path,
		result.Kind,
		fmt.Sprint(result.Rows),
		fmt.Sprint(result.DataRows),
		strings.Join(result.Columns, "|"),
	})
	if *flagSample > 0 {
		for i, sample := range result.Sample {
			_ = w.Write([]string{result.Path, "sample", fmt.Sprint(i + 1), "", sample})
		}
	}
}

func printInspectTable(result inspectResult) {
	fmt.Printf("%s\n", result.Path)
	if result.Err != "" {
		fmt.Printf("error\t%s\n", result.Err)
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "kind\t%s\n", result.Kind)
	if *flagRows || *flagInspect {
		fmt.Fprintf(tw, "rows\t%d\n", result.Rows)
		if result.DataRows > 0 {
			fmt.Fprintf(tw, "data_rows\t%d\n", result.DataRows)
		}
	}
	if (*flagColumns || *flagInspect) && len(result.Columns) > 0 {
		fmt.Fprintf(tw, "columns\t%s\n", strings.Join(result.Columns, ", "))
	}
	tw.Flush()
	if *flagSample <= 0 || len(result.Sample) == 0 {
		return
	}
	fmt.Println("sample")
	switch result.Kind {
	case "csv":
		printCSVSampleTable(result)
	case "json", "jsonl":
		printJSONSampleTable(result)
	default:
		printLineSampleTable(result)
	}
}

func printCSVSampleTable(result inspectResult) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if len(result.Columns) > 0 {
		fmt.Fprintln(tw, strings.Join(result.Columns, "\t"))
	}
	for i, sample := range result.Sample {
		if i == 0 && len(result.Columns) > 0 && sample == strings.Join(result.Columns, ",") {
			continue
		}
		values := splitCSVSample(sample)
		for i := range values {
			values[i] = tableCell(values[i])
		}
		fmt.Fprintln(tw, strings.Join(values, "\t"))
	}
	tw.Flush()
}

func printJSONSampleTable(result inspectResult) {
	if len(result.Columns) == 0 {
		printLineSampleTable(result)
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(result.Columns, "\t"))
	for _, sample := range result.Sample {
		var row map[string]any
		if err := json.Unmarshal([]byte(sample), &row); err != nil {
			continue
		}
		values := make([]string, 0, len(result.Columns))
		for _, col := range result.Columns {
			values = append(values, tableCell(inspectValueString(jsonField(row, col))))
		}
		fmt.Fprintln(tw, strings.Join(values, "\t"))
	}
	tw.Flush()
}

func printLineSampleTable(result inspectResult) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "row\ttext")
	for i, sample := range result.Sample {
		fmt.Fprintf(tw, "%d\t%s\n", i+1, tableCell(sample))
	}
	tw.Flush()
}

func splitCSVSample(sample string) []string {
	r := csv.NewReader(strings.NewReader(sample))
	r.FieldsPerRecord = -1
	rec, err := r.Read()
	if err != nil {
		return strings.Split(sample, ",")
	}
	return rec
}

func inspectValueString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64, bool:
		return fmt.Sprint(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	}
}

func tableCell(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)

	limit := *flagMaxCellWidth
	runes := []rune(s)
	if limit <= 0 || len(runes) <= limit {
		return s
	}
	if limit <= 3 {
		return strings.Repeat(".", limit)
	}
	return string(runes[:limit-3]) + "..."
}

func compactJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

func jsonKeys(raw []byte) []string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func selectedInspectFields() []string {
	if strings.TrimSpace(*flagSelect) == "" {
		return nil
	}
	fields := strings.Split(*flagSelect, ",")
	out := fields[:0]
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			out = append(out, field)
		}
	}
	return out
}

func inspectJSONKeys(raw []byte) []string {
	if selected := selectedInspectFields(); len(selected) > 0 {
		return selected
	}
	if !*flagFlatten {
		return jsonKeys(raw)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	flat := map[string]any{}
	flattenJSON("", obj, flat)
	keys := make([]string, 0, len(flat))
	for key := range flat {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func compactSelectedJSON(raw []byte) string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return compactJSON(raw)
	}
	if *flagFlatten {
		flat := map[string]any{}
		flattenJSON("", obj, flat)
		obj = flat
	}
	if selected := selectedInspectFields(); len(selected) > 0 {
		out := map[string]any{}
		for _, field := range selected {
			out[field] = jsonField(obj, field)
		}
		obj = out
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return compactJSON(raw)
	}
	return string(b)
}

func flattenJSON(prefix string, v any, out map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenJSON(key, child, out)
		}
	default:
		out[prefix] = x
	}
}

func jsonField(row map[string]any, field string) any {
	if v, ok := row[field]; ok {
		return v
	}
	parts := strings.Split(field, ".")
	var cur any = row
	for _, part := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func whereParts() (string, string, bool) {
	where := strings.TrimSpace(*flagWhere)
	if where == "" {
		return "", "", false
	}
	key, value, ok := strings.Cut(where, "=")
	return strings.TrimSpace(key), strings.TrimSpace(value), ok
}

func jsonRawMatches(raw []byte) bool {
	key, value, ok := whereParts()
	if !ok {
		return true
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return inspectValueString(jsonField(obj, key)) == value
}

func csvRecordMatches(columns, rec []string) bool {
	key, value, ok := whereParts()
	if !ok {
		return true
	}
	for i, col := range columns {
		if col == key && i < len(rec) {
			return rec[i] == value
		}
	}
	return false
}

func selectCSVRecord(columns, rec []string) []string {
	selected := selectedInspectFields()
	if len(selected) == 0 {
		return rec
	}
	out := make([]string, len(selected))
	for i, field := range selected {
		for j, col := range columns {
			if col == field && j < len(rec) {
				out[i] = rec[j]
				break
			}
		}
	}
	return out
}
