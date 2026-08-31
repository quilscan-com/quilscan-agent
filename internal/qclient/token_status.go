package qclient

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const defaultTokenStatusTimeout = 30 * time.Second

var totalBalancePattern = regexp.MustCompile(`(?m)^Total balance:\s+([+-]?[0-9]+(?:\.[0-9]+)?)\s+QUIL(?:\s|$)`)

type claimableRewardsResponse struct {
	BalanceQuil string `json:"balance_quil"`
}

// RunClaimableRewards returns qclient's exact JSON balance_quil decimal.
func RunClaimableRewards(ctx context.Context, req RunRequest, timeout time.Duration) (string, error) {
	args := []string{"token", "claimable-rewards", "--json"}
	output, err := runTokenStatusCommand(ctx, req, args, timeout)
	if err != nil {
		return "", err
	}

	var response claimableRewardsResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &response); err != nil {
		return "", fmt.Errorf("parse qclient token claimable-rewards JSON: %w", err)
	}
	if !decimalPattern.MatchString(response.BalanceQuil) {
		return "", fmt.Errorf("qclient token claimable-rewards returned invalid balance_quil %q", response.BalanceQuil)
	}
	return response.BalanceQuil, nil
}

// RunTokenBalance returns the exact decimal from qclient's Total balance line.
func RunTokenBalance(ctx context.Context, req RunRequest, timeout time.Duration) (string, error) {
	output, err := runTokenStatusCommand(ctx, req, []string{"token", "balance"}, timeout)
	if err != nil {
		return "", err
	}
	matches := totalBalancePattern.FindStringSubmatch(output)
	if len(matches) != 2 {
		return "", fmt.Errorf("qclient token balance output did not contain a Total balance line")
	}
	return matches[1], nil
}

func runTokenStatusCommand(ctx context.Context, req RunRequest, args []string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultTokenStatusTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if req.ConfigPath != "" {
		args = append(args, "--config", req.ConfigPath)
	}
	cmd := newCommand(ctx, req.BinaryPath, args...)
	if req.WorkDir != "" {
		cmd.Dir = req.WorkDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		output := strings.TrimSpace(string(out))
		if output != "" {
			return "", fmt.Errorf("run %s %s: %w: %s", req.BinaryPath, strings.Join(args, " "), err, output)
		}
		return "", fmt.Errorf("run %s %s: %w", req.BinaryPath, strings.Join(args, " "), err)
	}
	return string(out), nil
}
