package actions

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// urlPrefixSeparator is what we require between the trusted prefix and
// the rest of the download URL when validating client-supplied overrides.
// Without the trailing slash a malicious caller could pass
// "https://qstorage.quilibrium.com.attacker.com/..." which would still
// pass HasPrefix but resolve to attacker-controlled DNS.
const urlPrefixSeparator = "/"

// DefaultAgentReleaseURLPrefix is where the install script also reads the
// initial binary from. Frontend can override per-call via cmd args.
const DefaultAgentReleaseURLPrefix = "https://qstorage.quilibrium.com/quilscan-agent"

// AgentUpdaterDeps wires the self-update flow: download new binary, atomic
// rename onto the agent binary path, and request the platform service manager
// to restart the service. Restart happens out-of-band so the cmd_status reply
// makes it back to backend before this process is killed.
type AgentUpdaterDeps struct {
	AgentBinaryPath string        // e.g. /usr/local/bin/quilscan-agent
	Platform        string        // linux-amd64 / linux-arm64 / darwin-arm64
	SelfServiceUnit string        // systemd unit name OR launchd label
	Svc             SelfRestarter // service controller, used to bounce ourselves
}

// NewUpdateAgentHandler constructs the update_agent action.
//
// Args:
//
//	url     (string, optional) override download URL — defaults to
//	        DefaultAgentReleaseURLPrefix/quilscan-agent-${platform}
//	version (string, optional) target version, recorded in audit log only
func NewUpdateAgentHandler(d AgentUpdaterDeps) Handler {
	return func(c Command, emit Emitter) error {
		// SECURITY: the download URL must start with the compile-time
		// trusted prefix. A malicious actor with token control would
		// otherwise be able to point us at an attacker-controlled binary
		// and replace our own process — bypassing the entire 9-action
		// whitelist. The prefix is a Go `const`, not user-configurable
		// at runtime, so this check is grep-auditable.
		url, _ := c.Args["url"].(string)
		defaultURL := fmt.Sprintf("%s/quilscan-agent-%s", DefaultAgentReleaseURLPrefix, d.Platform)
		if url != "" && !strings.HasPrefix(url, DefaultAgentReleaseURLPrefix+urlPrefixSeparator) {
			err := fmt.Errorf("update_agent: refusing untrusted URL (must start with %s/)", DefaultAgentReleaseURLPrefix)
			emit(Status{ID: c.ID, Step: "rejected", Error: err.Error()})
			return err
		}
		if url == "" {
			url = defaultURL
		}

		emit(Status{ID: c.ID, Step: "downloading", Progress: 0.20})
		tmp := d.AgentBinaryPath + ".new"
		if err := downloadAgentBinary(url, tmp); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			os.Remove(tmp)
			return err
		}

		// Sanity check: don't ship empty / 4xx HTML downloads.
		if info, err := os.Stat(tmp); err == nil && info.Size() < 1024*1024 {
			os.Remove(tmp)
			err := fmt.Errorf("downloaded binary too small (%d bytes); refusing to install", info.Size())
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// Atomic rename. Linux's unlink-on-rename releases the old binary
		// inode while keeping the running process's mmap intact, so this is
		// safe to do without first stopping ourselves.
		emit(Status{ID: c.ID, Step: "swapping_binary", Progress: 0.70})
		if err := os.Rename(tmp, d.AgentBinaryPath); err != nil {
			os.Remove(tmp)
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// Done event before we restart — once the service manager kills us,
		// this WS frame might be the last thing that ever leaves this PID.
		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})

		// Schedule the service restart out-of-band so this goroutine's
		// exec.Command doesn't deadlock on a shutdown that's killing us.
		// The 500ms sleep gives the WS frame a chance to flush over TCP
		// before the process dies. svcctl.Ctl.Restart is non-blocking on
		// both platforms (systemctl --no-block on Linux, launchctl
		// kickstart -k on macOS).
		go func(unit string) {
			time.Sleep(500 * time.Millisecond)
			if d.Svc != nil {
				_ = d.Svc.Restart(unit)
			}
		}(d.SelfServiceUnit)

		return nil
	}
}

// downloadAgentBinary fetches url to dst with a 5min timeout. Streams to disk
// to avoid pulling a large binary fully into RAM.
func downloadAgentBinary(url, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}
