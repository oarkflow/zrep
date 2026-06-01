// Package output provides fast, buffered, optionally colored output.
// It uses a single goroutine to serialize writes, avoiding lock contention
// on stdout, and pre-formats byte slices directly into a 64 KiB buffer.
package output

import (
	"bufio"
	"fmt"
	"os"
	"strconv"

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
	Color        bool
	OnlyFiles    bool // -l
	OnlyMatching bool // -o
	Count        bool // -c
	NoFilename   bool // single file: omit filename prefix
	LineNumbers  bool // -n
	Context      int  // -C (before+after)
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
			fmt.Fprintf(p.bw, "error: %s: %v\n", r.Path, r.Err)
			if r.Cleanup != nil {
				r.Cleanup()
			}
			continue
		}
		if len(r.Matches) == 0 && len(r.OnlyMatch) == 0 && r.Count == 0 {
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

	if o.OnlyFiles {
		if o.Color {
			w.WriteString(colorGreen)
		}
		w.WriteString(r.Path)
		if o.Color {
			w.WriteString(colorReset)
		}
		w.WriteByte('\n')
		return
	}

	if o.Count {
		if !o.NoFilename {
			writeFilename(w, r.Path, o.Color)
			w.WriteByte(':')
		}
		writeInt(w, r.Count)
		w.WriteByte('\n')
		return
	}

	if o.OnlyMatching && len(r.OnlyMatch) > 0 {
		if !o.Color && !o.LineNumbers {
			line := make([]byte, 0, len(r.Path)+len(r.OnlyMatch)+2)
			if !o.NoFilename {
				line = append(line, r.Path...)
				line = append(line, ':')
			}
			line = append(line, r.OnlyMatch...)
			line = append(line, '\n')
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

	for _, lm := range r.Matches {
		if o.OnlyMatching {
			for _, match := range lm.Matches {
				p.writePrefix(r.Path, lm.LineNum)
				w.Write(lm.Line[match.Start:match.End])
				w.WriteByte('\n')
			}
			continue
		}
		// Filename prefix
		p.writePrefix(r.Path, lm.LineNum)
		// Line content with highlighted matches
		writeLineHighlighted(w, lm.Line, lm.Matches, o.Color)
		w.WriteByte('\n')
	}
}

func (p *Printer) writePrefix(path string, lineNum int) {
	o := &p.opts
	w := p.bw
	if !o.NoFilename {
		writeFilename(w, path, o.Color)
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
