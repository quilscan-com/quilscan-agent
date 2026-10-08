package qclient

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultTokenStatusTimeout = 30 * time.Second

var (
	decimalPattern         = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)
	totalBalancePattern    = regexp.MustCompile(`(?m)^Total balance:\s+([+-]?[0-9]+(?:\.[0-9]+)?)\s+QUIL(?:\s|$)`)
	baseUnitBalancePattern = regexp.MustCompile(`(?m)^([0-9]+) base units across [0-9]+ coins reported unspent by the configured node \(not a finalized balance\)\s*$`)
	claimableLinePattern   = regexp.MustCompile(`(?m)^Claimable prover rewards:\s+(.+?)\s*$`)
	claimableValuePattern  = regexp.MustCompile(`^([+-]?[0-9]+(?:\.[0-9]+)?)\s+QUIL \(witness cites global frame ([0-9]+); requires minting\)$`)
)

type ClaimableRewards struct {
	BalanceQuil string
	GlobalFrame uint64
	Known       bool
}

func RunClaimableRewards(ctx context.Context, req RunRequest, timeout time.Duration) (ClaimableRewards, error) {
	output, err := runTokenStatusCommand(ctx, req, []string{"token", "claimable-rewards"}, timeout)
	if err != nil {
		return ClaimableRewards{}, err
	}
	return parseClaimableRewards(output)
}

func parseClaimableRewards(output string) (ClaimableRewards, error) {
	line := claimableLinePattern.FindStringSubmatch(output)
	if len(line) != 2 {
		return ClaimableRewards{}, fmt.Errorf("qclient claimable-rewards output did not contain a Claimable prover rewards line")
	}
	payload := strings.TrimSpace(line[1])
	if strings.HasPrefix(payload, "unavailable (") {
		return ClaimableRewards{}, nil
	}
	claimable := claimableValuePattern.FindStringSubmatch(payload)
	if len(claimable) != 3 || !decimalPattern.MatchString(claimable[1]) {
		return ClaimableRewards{}, fmt.Errorf("qclient claimable-rewards output contained a malformed reward")
	}
	frame, err := strconv.ParseUint(claimable[2], 10, 64)
	if err != nil {
		return ClaimableRewards{}, fmt.Errorf("parse qclient claimable reward global frame: %w", err)
	}
	return ClaimableRewards{BalanceQuil: claimable[1], GlobalFrame: frame, Known: true}, nil
}

func RunTokenBalance(ctx context.Context, req RunRequest, timeout time.Duration) (string, error) {
	output, err := runTokenStatusCommand(ctx, req, []string{"token", "balance"}, timeout)
	if err != nil {
		return "", err
	}
	return parseTokenBalance(output)
}

func parseTokenBalance(output string) (string, error) {
	if baseUnits := baseUnitBalancePattern.FindStringSubmatch(output); len(baseUnits) == 2 {
		return tokenBaseUnitsToQuil(baseUnits[1])
	}
	if total := totalBalancePattern.FindStringSubmatch(output); len(total) == 2 {
		return total[1], nil
	}
	return "", fmt.Errorf("qclient token balance output did not contain a wallet balance line")
}

func tokenBaseUnitsToQuil(raw string) (string, error) {
	value, ok := new(big.Int).SetString(raw, 10)
	if !ok || value.Sign() < 0 {
		return "", fmt.Errorf("invalid token base-unit balance %q", raw)
	}
	scaled := new(big.Int).Mul(value, big.NewInt(125))
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(scaled, unit, fraction)
	fractionText := fraction.String()
	return whole.String() + "." + strings.Repeat("0", 12-len(fractionText)) + fractionText, nil
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
