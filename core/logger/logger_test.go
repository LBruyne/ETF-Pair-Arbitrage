package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Init writes to a file named etf_core_<timestamp>.log, honours the level and
// puts the source position on every line; a file that would pass its size
// limit is followed by a new one.
func TestInitWritesLeveledLinesWithSource(t *testing.T) {
	old := GlobalLogger
	defer func() { Close(); GlobalLogger, file = old, nil }()

	dir := t.TempDir()
	if err := Init(dir, "info", false); err != nil {
		t.Fatal(err)
	}
	GlobalLogger.Debug("store: hidden")
	GlobalLogger.Info("store: shown", "rows", 3)
	Close()

	files, _ := filepath.Glob(filepath.Join(dir, "etf_core_*.log"))
	if len(files) != 1 {
		t.Fatalf("log files = %v, want one etf_core_*.log", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if strings.Contains(text, "hidden") || !strings.Contains(text, `msg="store: shown" rows=3`) || !strings.Contains(text, "logger_test.go:") {
		t.Errorf("log file holds:\n%s", text)
	}

	if err := Init(dir, "loud", false); err == nil {
		t.Error("an unknown level must be refused")
	}
}

// A write that would take the file past its limit goes to a new file.
func TestRollingWriterRolls(t *testing.T) {
	w, err := newRollingWriter(t.TempDir(), "roll", 10)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	first := w.file.Name()
	if _, err := w.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if w.file.Name() != first {
		t.Fatal("rolled before the limit")
	}
	// Files are named by the second, so a roll within one second reopens the
	// same name: the roll is seen in the writer's size, which restarts with
	// the write that rolled.
	if _, err := w.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	if w.size != 8 {
		t.Errorf("size after the roll = %d, want 8", w.size)
	}
}
