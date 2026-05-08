package logstream

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

type Line struct {
	Msg string `json:"msg"`
}

type Source func(context.Context, string, int) (<-chan Line, <-chan error)

const maxLogLinesPerFlush = 100

type Streamer struct {
	// UnitName is the systemd unit on Linux. Used by JournalctlSource.
	UnitName string
	// LogPath, when non-empty, switches the default source to a file
	// tail. Set on macOS where the launchd plist redirects the node's
	// stdout/stderr to a flat file (no journald equivalent).
	LogPath string
	// Source overrides the default platform-aware source. Mostly used
	// in tests; production wires UnitName/LogPath and lets Start pick.
	Source Source

	mu     sync.Mutex
	cancel context.CancelFunc
	target string
}

func (s *Streamer) Start(ctx context.Context, target string, lines int, send func(map[string]interface{})) error {
	if target == "" {
		target = "node"
	}
	if target != "node" {
		return errors.New("unsupported log target")
	}
	if lines <= 0 {
		lines = 200
	}
	if lines > 500 {
		lines = 500
	}

	s.mu.Lock()
	if s.cancel != nil && s.target == target {
		s.mu.Unlock()
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.target = target
	s.mu.Unlock()

	source := s.Source
	if source == nil {
		if s.LogPath != "" {
			source = TailFileSource(s.LogPath)
		} else {
			source = JournalctlSource(s.UnitName)
		}
	}
	lineCh, errCh := source(ctx, target, lines)
	go s.run(ctx, target, lineCh, errCh, send)
	return nil
}

func (s *Streamer) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.cancel = nil
	s.target = ""
	s.mu.Unlock()
}

func (s *Streamer) run(ctx context.Context, target string, lineCh <-chan Line, errCh <-chan error, send func(map[string]interface{})) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := make([]Line, 0, maxLogLinesPerFlush)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		send(map[string]interface{}{
			"type":   "logs",
			"target": target,
			"lines":  batch,
		})
		batch = make([]Line, 0, maxLogLinesPerFlush)
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case line, ok := <-lineCh:
			if !ok {
				flush()
				return
			}
			if len(batch) < maxLogLinesPerFlush {
				batch = append(batch, line)
			}
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				send(map[string]interface{}{
					"type":   "logs",
					"target": target,
					"error":  err.Error(),
					"lines":  []Line{},
				})
			}
		case <-ticker.C:
			flush()
		}
	}
}

// TailFileSource streams a flat log file (the macOS path — launchd
// redirects the node's stdout/stderr there). `tail -n <lines> -f
// <path>` survives file rotation poorly but launchd doesn't rotate;
// the agent + node logs grow unbounded by design and are managed by
// the user. We pick `tail` over a hand-rolled file watcher because it
// handles the "file truncated then re-appended" case and the
// `-f` retry logic for free.
func TailFileSource(logPath string) Source {
	return func(ctx context.Context, target string, lines int) (<-chan Line, <-chan error) {
		out := make(chan Line, 100)
		errs := make(chan error, 1)
		cmd := exec.CommandContext(ctx, "tail", "-n", strconv.Itoa(lines), "-f", logPath)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errs <- err
			close(out)
			return out, errs
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			errs <- err
			close(out)
			return out, errs
		}
		if err := cmd.Start(); err != nil {
			errs <- err
			close(out)
			return out, errs
		}
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				out <- Line{Msg: scanner.Text()}
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				errs <- err
			}
		}()
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				out <- Line{Msg: scanner.Text()}
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				errs <- err
			}
		}()
		go func() {
			err := cmd.Wait()
			if err != nil && ctx.Err() == nil {
				errs <- err
			}
			close(out)
		}()
		return out, errs
	}
}

func JournalctlSource(unitName string) Source {
	if unitName == "" {
		unitName = "quilibrium-node.service"
	}
	return func(ctx context.Context, target string, lines int) (<-chan Line, <-chan error) {
		out := make(chan Line, 100)
		errs := make(chan error, 1)
		cmd := exec.CommandContext(ctx, "journalctl", "-u", unitName, "-n", strconv.Itoa(lines), "-f", "--no-pager", "--output=short-iso")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errs <- err
			close(out)
			return out, errs
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			errs <- err
			close(out)
			return out, errs
		}
		if err := cmd.Start(); err != nil {
			errs <- err
			close(out)
			return out, errs
		}
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				out <- Line{Msg: scanner.Text()}
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				errs <- err
			}
		}()
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				out <- Line{Msg: scanner.Text()}
			}
			if err := scanner.Err(); err != nil && ctx.Err() == nil {
				errs <- err
			}
		}()
		go func() {
			err := cmd.Wait()
			if err != nil && ctx.Err() == nil {
				errs <- err
			}
			close(out)
		}()
		return out, errs
	}
}
