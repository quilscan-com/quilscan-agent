package actions

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

func verifyEd25519BinarySignature(binaryPath, signaturePath, publicKeyBase64, label string) error {
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("read %s binary: %w", label, err)
	}
	signatureRaw, err := os.ReadFile(signaturePath)
	if err != nil {
		return fmt.Errorf("read %s signature: %w", label, err)
	}
	publicKeyText := strings.TrimSpace(publicKeyBase64)
	if publicKeyText == "" {
		publicKeyText = agentSigningPublicKeyBase64
	}
	publicKey, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		return fmt.Errorf("parse %s public key: %w", label, err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("%s public key has %d bytes, want %d", label, len(publicKey), ed25519.PublicKeySize)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureRaw)))
	if err != nil {
		return fmt.Errorf("parse %s signature: %w", label, err)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("%s signature has %d bytes, want %d", label, len(signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), binary, signature) {
		return fmt.Errorf("%s signature verification failed", label)
	}
	return nil
}
