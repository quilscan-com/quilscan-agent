package qclient

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const defaultTokenStatusTimeout = 30 * time.Second

var (
	decimalPattern        = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)
	totalBalancePattern   = regexp.MustCompile(`(?m)^Total balance:\s+([+-]?[0-9]+(?:\.[0-9]+)?)\s+QUIL(?:\s|$)`)
	claimableLinePattern  = regexp.MustCompile(`(?m)^Claimable prover rewards:\s+(.+?)\s*$`)
	claimableValuePattern = regexp.MustCompile(`^([+-]?[0-9]+(?:\.[0-9]+)?)\s+QUIL(?:\s+\(proven at global frame [0-9]+\))?$`)
)

const noRewardRecord = "unavailable (no reward record found)"

type TokenBalances struct {
	TokenBalanceQuil      string
	ClaimableRewardsQuil  string
	ClaimableRewardsKnown bool
}

// RunTokenBalances runs the official balance command once and extracts both
// the wallet balance and, when available, the claimable prover reward.
func RunTokenBalances(ctx context.Context, req RunRequest, timeout time.Duration) (TokenBalances, error) {
	output, err := runTokenStatusCommand(ctx, req, []string{"token", "balance"}, timeout)
	if err != nil {
		return TokenBalances{}, err
	}
	return parseTokenBalances(output)
}

func parseTokenBalances(output string) (TokenBalances, error) {
	total := totalBalancePattern.FindStringSubmatch(output)
	if len(total) != 2 {
		return TokenBalances{}, fmt.Errorf("qclient token balance output did not contain a Total balance line")
	}
	balances := TokenBalances{TokenBalanceQuil: total[1]}

	claimableLine := claimableLinePattern.FindStringSubmatch(output)
	if len(claimableLine) != 2 {
		return balances, nil
	}
	payload := strings.TrimSpace(claimableLine[1])
	if payload == noRewardRecord {
		balances.ClaimableRewardsQuil = "0.000000000000"
		balances.ClaimableRewardsKnown = true
		return balances, nil
	}
	if strings.HasPrefix(payload, "unavailable (") {
		return balances, nil
	}
	claimable := claimableValuePattern.FindStringSubmatch(payload)
	if len(claimable) != 2 || !decimalPattern.MatchString(claimable[1]) {
		return TokenBalances{}, fmt.Errorf("qclient token balance output contained malformed claimable rewards")
	}
	balances.ClaimableRewardsQuil = claimable[1]
	balances.ClaimableRewardsKnown = true
	return balances, nil
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
