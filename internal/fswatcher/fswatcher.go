// Package fswatcher emits file.created / file.modified / file.deleted events
// for the wrapper's session cwd, ignoring noisy generated directories (.git,
// node_modules, dist, etc.).
//
// Only the wrapper (`agentrun claude` / `agentrun codex`) uses this — global
// hook-based sessions don't have a long-lived process to run a watcher. That
// gap is documented; native sessions still get file CRUD signals via the
// agent's PostToolUse hook for Write/Edit tools.
package fswatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Emitter is the minimal interface the watcher needs from a recorder.
type Emitter interface {
	Emit(source, eventType string, payload []byte)
}

// Watcher streams filesystem change events to the recorder.
type Watcher struct {
	fsw  *fsnotify.Watcher
	rec  Emitter
	root string

	stop chan struct{}
	done chan struct{}

	mu       sync.Mutex
	closed   bool
	pending  map[string]time.Time // debounce: path -> first-seen-at
	debounce time.Duration
}

// IgnoredDirs is the set of directory base names skipped during the initial
// walk and ignored in subsequent events. Mirrors GOAL.md §14.4.
var IgnoredDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".next":        true,
	"dist":         true,
	"build":        true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	"target":       true,
	".gradle":      true,
	".cache":       true,
	".agentrun":    true,
}

// BinaryExtensions are file suffixes we DON'T hash or include in events
// (events are still emitted, just without a content hash for efficiency).
var BinaryExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".pdf": true, ".zip": true, ".tar": true, ".gz": true, ".tgz": true,
	".bz2": true, ".xz": true, ".7z": true, ".rar": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".o": true, ".a": true, ".bin": true, ".class": true,
	".pyc": true, ".pyo": true,
	".mp3": true, ".mp4": true, ".mov": true, ".webm": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
}

// debounceWindow batches rapid create+modify events for the same path. Editors
// often fire a write-then-modify sequence (e.g., vim writes a temp file then
// renames); we coalesce them into one event after the burst settles.
const debounceWindow = 200 * time.Millisecond

// New constructs a watcher rooted at root. The watcher is NOT started — call Start.
func New(root string, rec Emitter) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fswatcher: NewWatcher: %w", err)
	}
	return &Watcher{
		fsw:      fsw,
		rec:      rec,
		root:     root,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		pending:  make(map[string]time.Time),
		debounce: debounceWindow,
	}, nil
}

// Start walks the root tree to register dir watches, then spawns the event
// loop. Returns immediately on success. Errors during the initial walk are
// best-effort — partial coverage is better than no coverage.
func (w *Watcher) Start() error {
	_ = filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if IgnoredDirs[d.Name()] && path != w.root {
				return filepath.SkipDir
			}
			if addErr := w.fsw.Add(path); addErr != nil {
				// Non-fatal; logged once at debug level (stderr).
				fmt.Fprintf(os.Stderr, "agentrun fswatcher: add %s: %v\n", path, addErr)
			}
		}
		return nil
	})

	go w.run()
	return nil
}

// Close stops the watcher and waits for the goroutine to drain. Idempotent.
func (w *Watcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	close(w.stop)
	<-w.done
	return w.fsw.Close()
}

// run is the event loop. Drains fsnotify events into our normalised event
// shape and dispatches them via the recorder.
func (w *Watcher) run() {
	defer close(w.done)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-w.stop:
			w.flushPending(time.Now().Add(time.Hour)) // drain everything
			return

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "agentrun fswatcher: %v\n", err)

		case <-tick.C:
			w.flushPending(time.Now().Add(-w.debounce))
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	if w.isIgnored(ev.Name) {
		return
	}

	switch {
	case ev.Op&fsnotify.Create != 0:
		// If a directory was created, recursively watch it.
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.fsw.Add(ev.Name)
			return
		}
		w.enqueue(ev.Name)
	case ev.Op&fsnotify.Write != 0:
		w.enqueue(ev.Name)
	case ev.Op&fsnotify.Remove != 0:
		w.emit(ev.Name, "file.deleted", "", 0)
	case ev.Op&fsnotify.Rename != 0:
		w.emit(ev.Name, "file.deleted", "", 0)
	}
}

// enqueue adds a path to the pending debounce buffer.
func (w *Watcher) enqueue(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.pending[path]; !exists {
		w.pending[path] = time.Now()
	}
}

// flushPending emits events for any path whose first-seen-at is older than
// cutoff. Called both periodically (debounce) and on Close (drain).
func (w *Watcher) flushPending(cutoff time.Time) {
	w.mu.Lock()
	var paths []string
	for p, ts := range w.pending {
		if ts.Before(cutoff) {
			paths = append(paths, p)
			delete(w.pending, p)
		}
	}
	w.mu.Unlock()

	for _, p := range paths {
		w.classifyAndEmit(p)
	}
}

// classifyAndEmit stats the file and emits the appropriate file.* event.
func (w *Watcher) classifyAndEmit(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		// File vanished between event fire and our stat — treat as deleted.
		w.emit(path, "file.deleted", "", 0)
		return
	}
	if fi.IsDir() {
		return
	}

	var hash string
	if !isBinary(path) {
		hash = sha256File(path)
	}

	// file.created vs file.modified — we don't track previous state, so we
	// use a single "file.modified" event type. The caller can distinguish by
	// joining against earlier rows if they care.
	w.emit(path, "file.modified", hash, fi.Size())
}

// emit constructs the payload and forwards to the recorder.
func (w *Watcher) emit(path, eventType, hash string, size int64) {
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		rel = path
	}
	payload := map[string]any{
		"path":          rel,
		"absolute_path": path,
	}
	if hash != "" {
		payload["sha256"] = hash
	}
	if size > 0 {
		payload["size_bytes"] = size
	}
	b, _ := json.Marshal(payload)
	w.rec.Emit("filesystem", eventType, b)
}

// isIgnored returns true for paths inside an ignored directory or for files
// whose names look like editor temp files.
func (w *Watcher) isIgnored(path string) bool {
	rel, err := filepath.Rel(w.root, path)
	if err != nil {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if IgnoredDirs[part] {
			return true
		}
	}
	base := filepath.Base(path)
	if strings.HasSuffix(base, "~") || strings.HasPrefix(base, ".#") {
		return true
	}
	if strings.HasSuffix(base, ".swp") || strings.HasSuffix(base, ".swo") {
		return true
	}
	return false
}

func isBinary(path string) bool {
	return BinaryExtensions[strings.ToLower(filepath.Ext(path))]
}

func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
