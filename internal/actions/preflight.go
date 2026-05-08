package actions

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// preflightInstall runs platform-specific environment checks before the
// install handler downloads and starts the node binary. Returning a
// non-nil error causes the install handler to fail fast with a clear,
// actionable reason in cmd_status — instead of bombing ~90s later in
// the "configuring_rpc" step with a cryptic config.yml timeout.
//
// Currently only macOS has these gates. Linux Quilibrium binaries are
// statically linked and the systemd unit owns the host outright; on
// Mac users may have another local service on the node metrics port and may not have
// openssl@3 installed at all.
func preflightInstall(platform string) error {
	if !strings.HasPrefix(platform, "darwin") {
		return nil
	}
	if err := checkOpenSSL3(); err != nil {
		return err
	}
	if err := checkNodeMetricsPortFree(); err != nil {
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

// The node binary always opens a Prometheus listener on
// 127.0.0.1:49163 (we add `--prometheus-server 127.0.0.1:49163` for both
// fresh and migrated installs). If something else is already bound to
// that port, the node fatals immediately on startup. We probe by
// trying to listen ourselves; success → port is free, then we close.
func checkNodeMetricsPortFree() error {
	const addr = "127.0.0.1:49163"
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port 49163 is in use (Quilibrium needs it for the Prometheus endpoint) — find the conflicting process with:  lsof -nP -iTCP:49163 -sTCP:LISTEN  — typical culprits: a previous node instance or another local service")
	}
	// Tiny grace so the OS releases the bind before the install
	// proceeds and the node tries to bind for real. macOS is fast at
	// this; 10ms is empirically enough.
	_ = l.Close()
	time.Sleep(10 * time.Millisecond)
	return nil
}
