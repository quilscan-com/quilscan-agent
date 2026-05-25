package actions

import (
	"fmt"
	"os"
	"strings"
)

// preflightInstall runs platform-specific environment checks before the
// install handler downloads and starts the node binary. Returning a
// non-nil error causes the install handler to fail fast with a clear,
// actionable reason in cmd_status — instead of bombing ~90s later in
// the "configuring_rpc" step with a cryptic config.yml timeout.
//
// Currently only macOS has this gate. Linux Quilibrium binaries are
// statically linked; on Mac users may not have openssl@3 installed.
func preflightInstall(platform string) error {
	if !strings.HasPrefix(platform, "darwin") {
		return nil
	}
	if err := checkOpenSSL3(); err != nil {
		return err
	}
	return nil
}

// Quilibrium's official darwin-arm64 binary is dynamically linked
// against Homebrew openssl@3. Without it the node fails at dyld
// resolution before main() runs — visible in
// ~/Library/Logs/quilibrium-node.log as "Library not loaded".
func checkOpenSSL3() error {
	const dylib = "/opt/homebrew/opt/openssl@3/lib/libcrypto.3.dylib"
	if _, err := os.Stat(dylib); err == nil {
		return nil
	}
	return fmt.Errorf("Quilibrium node binary requires Homebrew openssl@3 — install Homebrew (https://brew.sh) then run:  brew install openssl@3")
}
