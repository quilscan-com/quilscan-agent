package qclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
	Snapshot         *StatusSnapshot
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

	args := []string{"node", "prover", "status", "--json"}
	run := func(args []string) ([]byte, string, error) {
		cmd := newCommand(ctx, req.BinaryPath, args...)
		if req.WorkDir != "" {
			cmd.Dir = req.WorkDir
		}
		cmd.WaitDelay = 100 * time.Millisecond
		out, err := cmd.Output()
		var exitErr *exec.ExitError
		diagnostic := ""
		if errors.As(err, &exitErr) {
			diagnostic = string(exitErr.Stderr)
			if len(diagnostic) > 4096 {
				diagnostic = diagnostic[:4096] + " [truncated]"
			}
		}
		return out, strings.TrimSpace(diagnostic), err
	}
	out, diagnostic, err := run(args)
	if err == nil {
		return ParseProverStatusJSON(string(out))
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("qclient prover status: %w", ctx.Err())
	}
	if !unsupportedStatusJSON(string(out) + "\n" + diagnostic) {
		return nil, fmt.Errorf("run %s %s: %w: %s", req.BinaryPath, strings.Join(args, " "), err, diagnostic)
	}
	out, diagnostic, err = run(args[:3])
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("qclient prover status legacy fallback: %w: %s", err, diagnostic)
	}
	return ParseProverStatus(string(out))
}

func unsupportedStatusJSON(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{"unexpected argument", "unrecognized option", "unknown flag", "unknown option", "flag provided but not defined"} {
		_, after, found := strings.Cut(lower, marker)
		if !found {
			continue
		}
		token := strings.TrimLeft(after, " :\t\r\n\"'`")
		if strings.HasPrefix(token, "--json") {
			suffix := strings.TrimPrefix(token, "--json")
			if suffix == "" || strings.ContainsRune(" \t\r\n\"'`,:", rune(suffix[0])) {
				return true
			}
		}
	}
	return false
}

const qclientSourceBuildSignatureArg = "--signature-check=false"

func newCommand(ctx context.Context, binaryPath string, args ...string) *exec.Cmd {
	// QuilScan verifies its release bundle during installation. Its source-built
	// clients use the native opt-out of official release self-checks per command.
	commandArgs := make([]string, len(args)+1)
	copy(commandArgs, args)
	commandArgs[len(args)] = qclientSourceBuildSignatureArg
	cmd := exec.CommandContext(ctx, binaryPath, commandArgs...)
	if env := commandEnv(os.Environ(), runtime.GOOS); env != nil {
		cmd.Env = env
	}
	return cmd
}

func commandEnv(env []string, goos string) []string {
	if goos != "darwin" {
		return nil
	}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "HOME" && value != "" {
			return nil
		}
	}
	updated := append([]string(nil), env...)
	for index, entry := range updated {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key == "HOME" {
			updated[index] = "HOME=/var/root"
			return updated
		}
	}
	return append(updated, "HOME=/var/root")
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
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[0], 10, 64)
	return n
}
