package actions

import (
	"crypto/ed25519"
	"encoding/base64"
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

const agentSigningPublicKeyBase64 = "CdZQuKKKObFcsQY61nhp4vZw7H6jBGBnlav7NuIh53k="

// AgentUpdaterDeps wires the self-update flow: download new binary, atomic
// rename onto the agent binary path, and request the platform service manager
// to restart the service. Restart happens out-of-band so the cmd_status reply
// makes it back to backend before this process is killed.
type AgentUpdaterDeps struct {
	AgentBinaryPath        string        // e.g. /usr/local/bin/quilscan-agent
	Platform               string        // linux-amd64 / linux-arm64 / darwin-arm64
	SelfServiceUnit        string        // systemd unit name OR launchd label
	Svc                    SelfRestarter // service controller, used to bounce ourselves
	ReleaseURLPrefix       string        // test override; defaults to DefaultAgentReleaseURLPrefix
	SigningPublicKeyBase64 string        // test override; defaults to agentSigningPublicKeyBase64
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
		releaseURLPrefix := strings.TrimRight(strings.TrimSpace(d.ReleaseURLPrefix), "/")
		if releaseURLPrefix == "" {
			releaseURLPrefix = DefaultAgentReleaseURLPrefix
		}
		url, _ := c.Args["url"].(string)
		defaultURL := fmt.Sprintf("%s/quilscan-agent-%s", releaseURLPrefix, d.Platform)
		if url != "" && !strings.HasPrefix(url, releaseURLPrefix+urlPrefixSeparator) {
			err := fmt.Errorf("update_agent: refusing untrusted URL (must start with %s/)", releaseURLPrefix)
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
		defer os.Remove(tmp)

		emit(Status{ID: c.ID, Step: "verifying_signature", Progress: 0.45})
		sigTmp := tmp + ".sig"
		if err := downloadAgentBinary(url+".sig", sigTmp); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		defer os.Remove(sigTmp)
		if err := verifyAgentBinarySignature(tmp, sigTmp, d.SigningPublicKeyBase64); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		if err := os.Chmod(tmp, 0o755); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// Sanity check: don't ship empty / 4xx HTML downloads.
		if info, err := os.Stat(tmp); err == nil && info.Size() < 1024*1024 {
			err := fmt.Errorf("downloaded binary too small (%d bytes); refusing to install", info.Size())
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// Atomic rename. Linux's unlink-on-rename releases the old binary
		// inode while keeping the running process's mmap intact, so this is
		// safe to do without first stopping ourselves.
		emit(Status{ID: c.ID, Step: "swapping_binary", Progress: 0.70})
		if err := os.Rename(tmp, d.AgentBinaryPath); err != nil {
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

func verifyAgentBinarySignature(binaryPath, signaturePath, publicKeyBase64 string) error {
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("read agent binary: %w", err)
	}
	signatureRaw, err := os.ReadFile(signaturePath)
	if err != nil {
		return fmt.Errorf("read agent signature: %w", err)
	}
	publicKeyText := strings.TrimSpace(publicKeyBase64)
	if publicKeyText == "" {
		publicKeyText = agentSigningPublicKeyBase64
	}
	publicKey, err := base64.StdEncoding.DecodeString(publicKeyText)
	if err != nil {
		return fmt.Errorf("parse agent public key: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("agent public key has %d bytes, want %d", len(publicKey), ed25519.PublicKeySize)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureRaw)))
	if err != nil {
		return fmt.Errorf("parse agent signature: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("agent signature has %d bytes, want %d", len(signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), binary, signature) {
		return fmt.Errorf("agent signature verification failed")
	}
	return nil
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
