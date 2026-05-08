// Package token generates and persists the agent's bearer token.
package token

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// Generate produces a 32-byte random token, std-base64 encoded.
func Generate() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Save writes the token at path with 0600 permissions.
func Save(path, tok string) error {
	return os.WriteFile(path, []byte(tok+"\n"), 0o600)
}

// Load reads the token file and returns the trimmed value.
func Load(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
