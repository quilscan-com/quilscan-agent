// Package peerinfo runs `${binary} --config <dir> --peer-info` when a config
// directory is supplied and extracts the local node connection count from the
// last "Peer N:" section header. If the command exits non-zero after printing
// peer sections, the parser still returns the count.
package peerinfo

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type RunRequest struct {
	BinaryPath string
	ConfigPath string
	WorkDir    string
}

var peerHeaderPattern = regexp.MustCompile(`^Peer\s+([0-9]+):\s*$`)

func Run(ctx context.Context, req RunRequest, timeout time.Duration) (int, bool, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, args := commandForRequest(ctx, req)
	out, err := cmd.CombinedOutput()
	count, ok := ParseConnectionCount(string(out))
	if ok {
		return count, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("run %s %v: %w", req.BinaryPath, args, err)
	}
	return count, ok, nil
}

func commandForRequest(ctx context.Context, req RunRequest) (*exec.Cmd, []string) {
	args := []string{}
	if req.ConfigPath != "" {
		args = append(args, "--config", req.ConfigPath)
	}
	args = append(args, "--peer-info")
	cmd := exec.CommandContext(ctx, req.BinaryPath, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	return cmd, args
}

func ParseConnectionCount(raw string) (int, bool) {
	sc := bufio.NewScanner(strings.NewReader(raw))
	last := 0
	ok := false
	for sc.Scan() {
		m := peerHeaderPattern.FindStringSubmatch(sc.Text())
		if len(m) != 2 {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		last = n
		ok = true
	}
	return last, ok
}
