// Package logger provides the process-wide GlobalLogger: a slog text logger
// that writes to size-rolled files under a directory and, optionally, to
// stdout as well.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MaxLogSize is the size at which a log file is closed and a new one opened.
const MaxLogSize = 50 * 1024 * 1024

// filePrefix starts every log file name: etf_core_<start time>.log.
const filePrefix = "etf_core"

// GlobalLogger writes to stdout until Init replaces it, so packages can log
// from their tests without any setup.
var GlobalLogger = slog.New(slog.NewTextHandler(os.Stdout, nil))

// file is the writer Init opened; nil before Init.
var file *rollingWriter

// Init routes GlobalLogger to dir/etf_core_<timestamp>.log, with the source
// position on every line, and to stdout too when console is set. level is one
// of debug, info, warn, error. Timestamps use time.Local, which main sets to
// UTC+8 before calling Init.
func Init(dir, level string, console bool) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return fmt.Errorf("log level %q: %w", level, err)
	}
	w, err := newRollingWriter(dir, filePrefix, MaxLogSize)
	if err != nil {
		return err
	}
	var out io.Writer = w
	if console {
		out = io.MultiWriter(os.Stdout, w)
	}
	GlobalLogger = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: lvl, AddSource: true}))
	file = w
	return nil
}

// Close closes the current log file. It does nothing before Init.
func Close() {
	if file != nil {
		_ = file.Close()
	}
}

// rollingWriter appends to one file and moves to a new timestamped file when
// the next write would take the current one past maxSize. Safe for
// concurrent use.
type rollingWriter struct {
	mu      sync.Mutex
	dir     string
	prefix  string
	maxSize int64
	file    *os.File
	size    int64
}

// newRollingWriter creates dir when it is missing and opens the first file.
func newRollingWriter(dir, prefix string, maxSize int64) (*rollingWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	w := &rollingWriter{dir: dir, prefix: prefix, maxSize: maxSize}
	return w, w.roll()
}

// roll closes the current file, if any, and opens the next one. The caller
// holds mu, or is the constructor.
func (w *rollingWriter) roll() error {
	if w.file != nil {
		_ = w.file.Close()
	}
	name := filepath.Join(w.dir, fmt.Sprintf("%s_%s.log", w.prefix, time.Now().Format("20060102_150405")))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w.file, w.size = f, 0
	return nil
}

// Write appends p, rolling to a new file first when p would not fit.
func (w *rollingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxSize {
		if err := w.roll(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close closes the current file.
func (w *rollingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
