package qclient

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type ProverStatus struct {
	PeerID           string
	Version          string
	Seniority        int64
	PeerScore        int64
	RunningWorkers   int64
	AllocatedWorkers int64
	LastReceived     int64
	Reachable        bool
}

type RunRequest struct {
	BinaryPath string
	ConfigPath string
	WorkDir    string
}

func Run(ctx context.Context, req RunRequest, timeout time.Duration) (*ProverStatus, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{}
	if req.ConfigPath != "" {
		args = append(args, "--config", req.ConfigPath)
	}
	args = append(args, "node", "prover", "status")
	cmd := exec.CommandContext(ctx, req.BinaryPath, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run %s %s: %w", req.BinaryPath, strings.Join(args, " "), err)
	}
	return ParseProverStatus(string(out))
}

func ParseProverStatus(raw string) (*ProverStatus, error) {
	status := &ProverStatus{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Peer ID":
			status.PeerID = val
		case "Version":
			status.Version = val
		case "Seniority":
			status.Seniority = atoi64(val)
		case "Peer Score":
			status.PeerScore = atoi64(val)
		case "Running Workers":
			status.RunningWorkers = atoi64(val)
		case "Allocated Workers":
			status.AllocatedWorkers = atoi64(val)
		case "Last Received":
			status.LastReceived = atoi64(val)
		case "Reachable":
			status.Reachable = strings.EqualFold(val, "true")
		}
	}
	if err := sc.Err(); err != nil {
		return status, err
	}
	if status.PeerID == "" && status.Version == "" {
		return status, fmt.Errorf("qclient prover status did not contain node identity")
	}
	return status, nil
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
