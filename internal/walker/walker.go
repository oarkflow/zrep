// Package walker provides parallel directory traversal.
package walker

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// Entry is a file discovered during traversal.
type Entry struct {
	Path string
	Info fs.FileInfo
}

// Filter decides whether to include a path.
type Filter func(path string, d fs.DirEntry) bool

// DefaultFilter skips hidden and common noise directories.
var DefaultFilter Filter = func(path string, d fs.DirEntry) bool {
	name := d.Name()
	if len(name) == 0 {
		return true
	}
	if name[0] == '.' {
		return false
	}
	if IsDefaultIgnoredName(name) {
		return false
	}
	return true
}

// IsDefaultIgnoredName reports whether name is one of the noisy directories
// skipped by zrep unless -hidden or -no-default-ignore is set.
func IsDefaultIgnoredName(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", ".git", ".hg", ".svn",
		"__pycache__", ".mypy_cache", "dist", "build", ".cache":
		return true
	}
	return false
}

// Walker walks directories in parallel.
type Walker struct {
	Filter  Filter
	Follow  bool
	Results chan Entry

	workers int
	wg      sync.WaitGroup
	active  int64 // number of in-flight work items
	queue   chan string
	seen    sync.Map
}

// New creates a Walker.
func New(workers int) *Walker {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0) * 2
	}
	w := &Walker{
		Filter:  DefaultFilter,
		Results: make(chan Entry, workers*64),
		queue:   make(chan string, workers*256),
		workers: workers,
	}
	for i := 0; i < workers; i++ {
		w.wg.Add(1)
		go w.worker()
	}
	return w
}

// Walk enqueues root paths for traversal.
func (w *Walker) Walk(roots ...string) {
	for _, root := range roots {
		atomic.AddInt64(&w.active, 1)
		w.queue <- root
	}
}

// Wait blocks until traversal is complete, then closes Results.
func (w *Walker) Wait() {
	// Wait until all work drains via active counter
	// Workers signal done via wg
	w.wg.Wait()
	close(w.Results)
}

func (w *Walker) worker() {
	defer w.wg.Done()
	for path := range w.queue {
		w.processPath(path)
		// Decrement active; if zero, close queue to signal all workers to exit
		if atomic.AddInt64(&w.active, -1) == 0 {
			// Safe to close: no more senders (all active==0 means no pending sends)
			close(w.queue)
			return
		}
	}
}

func (w *Walker) enqueue(path string) {
	atomic.AddInt64(&w.active, 1)
	select {
	case w.queue <- path:
	default:
		// Queue full: process synchronously to avoid blocking
		w.processPath(path)
		atomic.AddInt64(&w.active, -1)
	}
}

func (w *Walker) processPath(path string) {
	if w.Follow {
		if realPath, err := filepath.EvalSymlinks(path); err == nil {
			if _, loaded := w.seen.LoadOrStore(realPath, true); loaded {
				return
			}
			path = realPath
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return
	}

	if !fi.IsDir() {
		f.Close()
		if fi.Mode()&fs.ModeType == 0 {
			w.Results <- Entry{Path: path, Info: fi}
		}
		return
	}

	const batchSize = 512
	for {
		entries, err := f.ReadDir(batchSize)
		for _, de := range entries {
			childPath := filepath.Join(path, de.Name())
			if de.Type()&fs.ModeSymlink != 0 && w.Follow {
				if realPath, err := filepath.EvalSymlinks(childPath); err == nil {
					childPath = realPath
					if info, err := os.Stat(childPath); err == nil {
						de = fileInfoDirEntry{info: info}
					}
				}
			}
			filter := w.Filter
			if filter != nil && !filter(childPath, de) {
				continue
			}
			if de.IsDir() {
				w.enqueue(childPath)
			} else {
				info, e2 := de.Info()
				if e2 != nil {
					continue
				}
				if info.Mode()&fs.ModeType != 0 {
					continue
				}
				w.Results <- Entry{Path: childPath, Info: info}
			}
		}
		if err != nil || len(entries) < batchSize {
			break
		}
	}
	f.Close()
}

type fileInfoDirEntry struct {
	info fs.FileInfo
}

func (d fileInfoDirEntry) Name() string               { return d.info.Name() }
func (d fileInfoDirEntry) IsDir() bool                { return d.info.IsDir() }
func (d fileInfoDirEntry) Type() fs.FileMode          { return d.info.Mode().Type() }
func (d fileInfoDirEntry) Info() (fs.FileInfo, error) { return d.info, nil }
