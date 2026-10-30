package qclient

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type TokenSection string

const (
	TokenRewards      TokenSection = "rewards"
	TokenLegacy       TokenSection = "legacy"
	TokenConfidential TokenSection = "confidential"
	// Balance discovery may span many pages; only the caller's cancellation
	// ends the default run. Positive explicit timeouts remain supported.
	DefaultTokenStatusTimeout time.Duration = 0
)

type TokenSectionResult struct {
	Section     TokenSection
	BalanceQuil string
	Known       bool
	ReadState   string
	Error       string
	GlobalFrame uint64
}

var (
	tokenSectionDecimal     = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)
	legacyDecimalLine       = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?) QUIL \(([0-9]+) base units\) across [0-9]+ coins \([0-9]+ already shielded\)$`)
	legacyRawLine           = regexp.MustCompile(`^([0-9]+) base units across [0-9]+ coins \([0-9]+ already shielded\)$`)
	legacyShieldedNone      = regexp.MustCompile(`^none left \([0-9]+ already shielded\)$`)
	confidentialDecimalLine = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?) QUIL \(([0-9]+) base units\) across [0-9]+ coins reported unspent by the configured node \(not a finalized balance\)$`)
	confidentialRawLine     = regexp.MustCompile(`^([0-9]+) base units across [0-9]+ coins reported unspent by the configured node \(not a finalized balance\)$`)
)

// RunTokenStatus reports each section as its line arrives. A duplicate line
// invalidates that section with a second callback. Process errors are returned
// separately and never invalidate a section already reported successfully.
func RunTokenStatus(ctx context.Context, req RunRequest, timeout time.Duration, onResult func(TokenSectionResult)) error {
	if timeout <= 0 {
		timeout = DefaultTokenStatusTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	args := []string{"token", "claimable-rewards", "--config", req.ConfigPath}
	cmd := newCommand(ctx, req.BinaryPath, args...)
	cmd.Dir = req.WorkDir
	// os/exec owns the copying goroutines and closes inherited pipes after this
	// delay, including when a descendant outlives the qclient process.
	cmd.WaitDelay = 100 * time.Millisecond
	stream := &tokenSectionStream{onResult: onResult, seen: make(map[TokenSection]int)}
	diagnostic := &tokenDiagnosticWriter{}
	cmd.Stdout = stream
	cmd.Stderr = diagnostic
	err := cmd.Run()
	stream.finishLine()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		err = fmt.Errorf("qclient token claimable-rewards: %w%s", err, diagnostic.suffix())
	}
	for _, section := range []TokenSection{TokenRewards, TokenLegacy, TokenConfidential} {
		if stream.seen[section] != 0 {
			continue
		}
		r := TokenSectionResult{Section: section, ReadState: "unsupported", Error: "qclient output did not contain this section"}
		if err != nil {
			r.ReadState = "error"
			r.Error = err.Error()
		}
		stream.emit(r)
	}
	return err
}

const maxTokenSectionLine = 64 * 1024

// tokenSectionStream frames stdout incrementally with bounded storage. An
// oversized line is drained through its newline so later sections still arrive.
type tokenSectionStream struct {
	onResult  func(TokenSectionResult)
	seen      map[TokenSection]int
	line      []byte
	oversized bool
}

func (s *tokenSectionStream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		fragment := p
		if end >= 0 {
			fragment = p[:end]
		}
		remaining := maxTokenSectionLine - len(s.line)
		if len(fragment) > remaining {
			s.line = append(s.line, fragment[:remaining]...)
			s.oversized = true
		} else {
			s.line = append(s.line, fragment...)
		}
		if end < 0 {
			break
		}
		s.finishLine()
		p = p[end+1:]
	}
	return n, nil
}

func (s *tokenSectionStream) finishLine() {
	line := strings.TrimSpace(string(s.line))
	section, payload := tokenSectionPayload(line)
	if section != "" {
		s.seen[section]++
		switch s.seen[section] {
		case 1:
			if s.oversized {
				s.emit(TokenSectionResult{Section: section, ReadState: "error", Error: "qclient section line exceeds 64 KiB"})
			} else {
				s.emit(parseTokenSection(section, payload))
			}
		case 2:
			s.emit(TokenSectionResult{Section: section, ReadState: "error", Error: "qclient output contained duplicate section lines"})
		}
	}
	s.line = s.line[:0]
	s.oversized = false
}

func (s *tokenSectionStream) emit(r TokenSectionResult) {
	if s.onResult != nil {
		s.onResult(r)
	}
}

func tokenSectionPayload(line string) (TokenSection, string) {
	for _, prefix := range []struct {
		text    string
		section TokenSection
	}{
		{"Claimable prover rewards:", TokenRewards},
		{"Unshielded legacy coins:", TokenLegacy},
		{"Confidential QUIL coins:", TokenConfidential},
		{"Confidential coins:", TokenConfidential},
	} {
		if payload, ok := strings.CutPrefix(line, prefix.text); ok {
			return prefix.section, strings.TrimSpace(payload)
		}
	}
	return "", ""
}

func parseTokenSection(section TokenSection, payload string) TokenSectionResult {
	r := TokenSectionResult{Section: section, ReadState: "error", Error: "qclient output contained a malformed " + string(section) + " section"}
	if reason, ok := strings.CutPrefix(payload, "unavailable ("); ok && strings.HasSuffix(reason, ")") {
		reason = strings.TrimSuffix(reason, ")")
		if reason != "" {
			r.ReadState, r.Error = "unavailable", reason
		}
		return r
	}
	if section == TokenRewards {
		value := claimableValuePattern.FindStringSubmatch(payload)
		if len(value) != 3 || !tokenSectionDecimal.MatchString(value[1]) {
			return r
		}
		frame, err := strconv.ParseUint(value[2], 10, 64)
		if err != nil {
			return r
		}
		r.BalanceQuil, r.GlobalFrame = value[1], frame
	} else {
		none := false
		decimalPattern, rawPattern := legacyDecimalLine, legacyRawLine
		if section == TokenLegacy {
			none = payload == "none (the listing has no coin under this identity's key or peer-id owner)" || payload == "none (this node has no Ed448 identity)" || legacyShieldedNone.MatchString(payload)
		} else {
			none = payload == "none exist on the network yet (no QUIL root published)" || payload == "none exist in this application yet (no root published)"
			decimalPattern, rawPattern = confidentialDecimalLine, confidentialRawLine
		}
		if none {
			return TokenSectionResult{Section: section, Known: true, BalanceQuil: "0", ReadState: "none"}
		}
		if value := decimalPattern.FindStringSubmatch(payload); len(value) == 3 {
			// Check that the displayed decimal and exact raw amount agree while
			// preserving the original decimal string in the result.
			decimal, ok := new(big.Rat).SetString(value[1])
			units, rawOK := new(big.Int).SetString(value[2], 10)
			if !ok || !rawOK || decimal.Cmp(new(big.Rat).SetFrac(units, big.NewInt(8000000000))) != 0 {
				return r
			}
			r.BalanceQuil = value[1]
		} else if value := rawPattern.FindStringSubmatch(payload); len(value) == 2 {
			balance, err := tokenBaseUnitsToQuil(value[1])
			if err != nil {
				return r
			}
			r.BalanceQuil = balance
		} else {
			return r
		}
	}
	r.Known, r.ReadState, r.Error = true, "ok", ""
	return r
}

type tokenDiagnosticWriter struct {
	data      []byte
	truncated bool
}

func (w *tokenDiagnosticWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 4096 - len(w.data)
	if len(p) > remaining {
		p = p[:remaining]
		w.truncated = true
	}
	w.data = append(w.data, p...)
	return n, nil
}

func (w *tokenDiagnosticWriter) suffix() string {
	diagnostic := strings.TrimSpace(string(w.data))
	if w.truncated {
		diagnostic += " [truncated]"
	}
	if diagnostic == "" {
		return ""
	}
	return ": " + diagnostic
}
