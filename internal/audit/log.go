// Package audit appends a human-readable record of every command the
// agent receives. The path is platform-specific — /var/log on Linux
// (root-owned) and ~/Library/Logs on macOS (user-owned). New logs are created
// owner-readable only so multi-user hosts cannot read another operator's
// agent activity.
package audit

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Writer is a thread-safe, append-only audit log.
type Writer struct {
	mu sync.Mutex
	f  *os.File
}

// New opens path in append mode (0600), creating if missing.
func New(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f}, nil
}

// Close flushes and closes the file.
func (w *Writer) Close() error { return w.f.Close() }

// Record writes one line: RFC3339 | action | result | k=v k=v ...
func (w *Writer) Record(action, result string, fields map[string]string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	parts := []string{time.Now().UTC().Format(time.RFC3339), action, result}
	for k, v := range fields {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	_, err := fmt.Fprintln(w.f, strings.Join(parts, " | "))
	return err
}
