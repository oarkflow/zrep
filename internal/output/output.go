// Package output provides fast, buffered, optionally colored output.
// It uses a single goroutine to serialize writes, avoiding lock contention
// on stdout, and pre-formats byte slices directly into a 64 KiB buffer.
package output

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zrep/zrep/internal/matcher"
	"github.com/zrep/zrep/internal/searcher"
)

// ANSI color codes
const (
	colorReset   = "\033[0m"
	colorBold    = "\033[1m"
	colorRed     = "\033[1;31m"
	colorGreen   = "\033[0;32m"
	colorYellow  = "\033[1;33m"
	colorCyan    = "\033[0;36m"
	colorMagenta = "\033[0;35m"
)

// Options controls output formatting.
type Options struct {
	Color             bool
	OnlyFiles         bool // -l
	OnlyMatching      bool // -o
	Count             bool // -c
	NoFilename        bool // single file: omit filename prefix
	LineNumbers       bool // -n
	Context           int  // -C (before+after)
	Mode              string
	Heading           bool
	Replace           string
	FilesWithout      bool
	IncludeZero       bool
	Null              bool
	PathSeparator     string
	Trim              bool
	MaxColumns        int
	MaxColumnsPreview bool
	NoMessages        bool
	Quiet             bool
}

// Printer serializes results to stdout via a single goroutine.
type Printer struct {
	opts Options
	in   chan searcher.Result
	done chan struct{}
	bw   *bufio.Writer
}

// New creates a Printer and starts its background writer goroutine.
func New(opts Options, bufSize int) *Printer {
	if bufSize <= 0 {
		bufSize = 64 << 10
	}
	p := &Printer{
		opts: opts,
		in:   make(chan searcher.Result, 256),
		done: make(chan struct{}),
		bw:   bufio.NewWriterSize(os.Stdout, bufSize),
	}
	go p.run()
	return p
}

// Send enqueues a result for printing.
func (p *Printer) Send(r searcher.Result) { p.in <- r }

// Close signals the printer to flush and exit. Blocks until done.
func (p *Printer) Close() {
	close(p.in)
	<-p.done
}

func (p *Printer) run() {
	defer func() {
		p.bw.Flush() //nolint:errcheck
		close(p.done)
	}()

	for r := range p.in {
		if r.Err != nil {
			if !p.opts.NoMessages && !p.opts.Quiet {
				fmt.Fprintf(p.bw, "error: %s: %v\n", p.formatPath(r.Path), r.Err)
			}
			if r.Cleanup != nil {
				r.Cleanup()
			}
			continue
		}
		if !r.Force && len(r.Matches) == 0 && len(r.OnlyMatch) == 0 && r.Count == 0 {
			if r.Cleanup != nil {
				r.Cleanup()
			}
			continue
		}
		p.printResult(r)
		if r.Cleanup != nil {
			r.Cleanup()
		}
	}
}

func (p *Printer) printResult(r searcher.Result) {
	o := &p.opts
	w := p.bw
	if o.Quiet {
		return
	}

	if o.OnlyFiles {
		if o.Color {
			w.WriteString(colorGreen)
		}
		w.WriteString(p.formatPath(r.Path))
		if o.Color {
			w.WriteString(colorReset)
		}
		p.writeRecordEnd()
		return
	}

	if o.Count {
		if !o.NoFilename {
			writeFilename(w, p.formatPath(r.Path), o.Color)
			w.WriteByte(':')
		}
		writeInt(w, r.Count)
		p.writeRecordEnd()
		return
	}
	if o.Mode == "json-events" {
		p.printJSONEvents(r)
		return
	}
	if o.Mode == "json" {
		p.printJSON(r)
		return
	}
	if o.Mode == "vimgrep" {
		p.printVimgrep(r)
		return
	}

	if o.OnlyMatching && len(r.OnlyMatch) > 0 {
		if !o.Color && !o.LineNumbers {
			line := make([]byte, 0, len(r.Path)+len(r.OnlyMatch)+2)
			if !o.NoFilename {
				line = append(line, p.formatPath(r.Path)...)
				line = append(line, ':')
			}
			line = append(line, r.OnlyMatch...)
			if o.Null {
				line = append(line, 0)
			} else {
				line = append(line, '\n')
			}
			for i := 0; i < r.Count; i++ {
				w.Write(line)
			}
			return
		}
		for i := 0; i < r.Count; i++ {
			p.writePrefix(r.Path, 0)
			w.Write(r.OnlyMatch)
			w.WriteByte('\n')
		}
		return
	}

	if o.Heading && !o.NoFilename {
		writeFilename(w, p.formatPath(r.Path), o.Color)
		p.writeRecordEnd()
	}
	for _, lm := range r.Matches {
		lm = p.prepareLine(lm)
		if o.OnlyMatching {
			for _, match := range lm.Matches {
				p.writePrefix(r.Path, lm.LineNum)
				w.Write(lm.Line[match.Start:match.End])
				p.writeRecordEnd()
			}
			continue
		}
		p.writeLocation(r.Path, lm)
		if p.omitLongLine(lm) {
			fmt.Fprintf(w, "    [%d columns omitted]\n", len(lm.Line))
			continue
		}
		w.WriteString("    ")
		if o.Replace != "" && len(lm.Matches) > 0 && !lm.Context {
			writeLineReplaced(w, lm.Line, lm.Matches, []byte(o.Replace), o.Color)
		} else {
			writeLineHighlighted(w, lm.Line, lm.Matches, o.Color)
		}
		p.writeRecordEnd()
	}
}

func (p *Printer) printJSON(r searcher.Result) {
	w := p.bw
	for _, lm := range r.Matches {
		startCol, endCol := matchColumns(lm)
		row := struct {
			Path    string `json:"path"`
			Line    int    `json:"line"`
			Column  int    `json:"column"`
			End     int    `json:"end_column"`
			Context bool   `json:"context"`
			Text    string `json:"text"`
		}{
			Path:    p.formatPath(r.Path),
			Line:    lm.LineNum,
			Column:  startCol,
			End:     endCol,
			Context: lm.Context,
			Text:    string(lm.Line),
		}
		b, err := json.Marshal(row)
		if err != nil {
			continue
		}
		w.Write(b)
		w.WriteByte('\n')
	}
}

func (p *Printer) printJSONEvents(r searcher.Result) {
	w := p.bw
	p.writeJSONEvent("begin", map[string]any{"path": dataValue([]byte(p.formatPath(r.Path)))})
	for _, lm := range r.Matches {
		startCol, endCol := matchColumns(lm)
		typ := "match"
		if lm.Context {
			typ = "context"
		}
		p.writeJSONEvent(typ, map[string]any{
			"path":       dataValue([]byte(p.formatPath(r.Path))),
			"line":       lm.LineNum,
			"column":     startCol,
			"end_column": endCol,
			"text":       dataValue(lm.Line),
		})
	}
	p.writeJSONEvent("end", map[string]any{"path": dataValue([]byte(p.formatPath(r.Path))), "matches": r.Count})
	w.Flush() //nolint:errcheck
}

func (p *Printer) writeJSONEvent(typ string, data map[string]any) {
	row := map[string]any{"type": typ, "data": data}
	b, err := json.Marshal(row)
	if err != nil {
		return
	}
	p.bw.Write(b)
	p.bw.WriteByte('\n')
}

func (p *Printer) printVimgrep(r searcher.Result) {
	w := p.bw
	for _, lm := range r.Matches {
		startCol, _ := matchColumns(lm)
		w.WriteString(p.formatPath(r.Path))
		w.WriteByte(':')
		writeInt(w, lm.LineNum)
		w.WriteByte(':')
		writeInt(w, startCol)
		w.WriteByte(':')
		w.Write(lm.Line)
		p.writeRecordEnd()
	}
}

func (p *Printer) writePrefix(path string, lineNum int) {
	o := &p.opts
	w := p.bw
	if !o.NoFilename {
		writeFilename(w, p.formatPath(path), o.Color)
		w.WriteByte(':')
	}
	if o.LineNumbers {
		if o.Color {
			w.WriteString(colorCyan)
		}
		writeInt(w, lineNum)
		if o.Color {
			w.WriteString(colorReset)
		}
		w.WriteByte(':')
	}
}

func writeInt(w *bufio.Writer, n int) {
	var buf [20]byte
	w.Write(strconv.AppendInt(buf[:0], int64(n), 10))
}

func (p *Printer) writeLocation(path string, lm matcher.LineMatch) {
	w := p.bw
	if !p.opts.NoFilename && !p.opts.Heading {
		writeFilename(w, p.formatPath(path), p.opts.Color)
		w.WriteByte(':')
	}
	if p.opts.Color {
		w.WriteString(colorCyan)
	}
	writeInt(w, lm.LineNum)
	if p.opts.Color {
		w.WriteString(colorReset)
	}
	w.WriteByte(':')
	startCol, endCol := matchColumns(lm)
	if p.opts.Color {
		w.WriteString(colorYellow)
	}
	writeInt(w, startCol)
	if endCol > startCol {
		w.WriteByte('-')
		writeInt(w, endCol)
	}
	if p.opts.Color {
		w.WriteString(colorReset)
	}
	w.WriteByte('\n')
}

func (p *Printer) writeRecordEnd() {
	if p.opts.Null {
		p.bw.WriteByte(0)
		return
	}
	p.bw.WriteByte('\n')
}

func (p *Printer) formatPath(path string) string {
	sep := p.opts.PathSeparator
	if sep == "" {
		return path
	}
	path = strings.ReplaceAll(path, "/", sep)
	path = strings.ReplaceAll(path, "\\", sep)
	return path
}

func (p *Printer) prepareLine(lm matcher.LineMatch) matcher.LineMatch {
	if p.opts.Trim {
		n := 0
		for n < len(lm.Line) {
			switch lm.Line[n] {
			case ' ', '\t', '\r', '\n', '\v', '\f':
				n++
			default:
				goto done
			}
		}
	done:
		if n > 0 {
			lm.Line = lm.Line[n:]
			for i := range lm.Matches {
				lm.Matches[i].Start -= n
				lm.Matches[i].End -= n
				if lm.Matches[i].Start < 0 {
					lm.Matches[i].Start = 0
				}
				if lm.Matches[i].End < lm.Matches[i].Start {
					lm.Matches[i].End = lm.Matches[i].Start
				}
				if lm.Matches[i].End > len(lm.Line) {
					lm.Matches[i].End = len(lm.Line)
				}
			}
		}
	}
	if p.opts.MaxColumns > 0 && p.opts.MaxColumnsPreview && len(lm.Line) > p.opts.MaxColumns {
		lm.Line = lm.Line[:p.opts.MaxColumns]
		for i := range lm.Matches {
			if lm.Matches[i].Start > len(lm.Line) {
				lm.Matches[i].Start = len(lm.Line)
			}
			if lm.Matches[i].End > len(lm.Line) {
				lm.Matches[i].End = len(lm.Line)
			}
		}
	}
	return lm
}

func (p *Printer) omitLongLine(lm matcher.LineMatch) bool {
	return p.opts.MaxColumns > 0 && !p.opts.MaxColumnsPreview && len(lm.Line) > p.opts.MaxColumns
}

func dataValue(b []byte) map[string]string {
	if utf8.Valid(b) {
		return map[string]string{"text": string(b)}
	}
	return map[string]string{"bytes": base64.StdEncoding.EncodeToString(b)}
}

func matchColumns(lm matcher.LineMatch) (int, int) {
	if len(lm.Matches) == 0 || lm.Context {
		return 1, 1
	}
	first := lm.Matches[0]
	start := first.Start + 1
	end := first.End
	if end < start {
		end = start
	}
	return start, end
}

func writeFilename(w *bufio.Writer, path string, color bool) {
	if color {
		w.WriteString(colorMagenta)
	}
	w.WriteString(path)
	if color {
		w.WriteString(colorReset)
	}
}

// writeLineHighlighted writes the line with matched ranges highlighted in red.
func writeLineHighlighted(w *bufio.Writer, line []byte, matches []matcher.Match, color bool) {
	if !color || len(matches) == 0 {
		w.Write(line)
		return
	}
	pos := 0
	for _, m := range matches {
		if m.Start > pos {
			w.Write(line[pos:m.Start])
		}
		w.WriteString(colorRed)
		w.Write(line[m.Start:m.End])
		w.WriteString(colorReset)
		pos = m.End
	}
	if pos < len(line) {
		w.Write(line[pos:])
	}
}

func writeLineReplaced(w *bufio.Writer, line []byte, matches []matcher.Match, replacement []byte, color bool) {
	pos := 0
	for _, m := range matches {
		if m.Start > pos {
			w.Write(line[pos:m.Start])
		}
		if color {
			w.WriteString(colorGreen)
		}
		w.Write(replacement)
		if color {
			w.WriteString(colorReset)
		}
		pos = m.End
	}
	if pos < len(line) {
		w.Write(line[pos:])
	}
}
