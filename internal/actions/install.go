package actions

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/release"
	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
	"github.com/quilscan-com/quilscan-agent/internal/systemd"
)

// Downloader is the interface install.go uses — abstracted so tests can fake.
type Downloader interface {
	Download(version, platform, destDir string) error
}

// SystemdOps is the service-definition writer/controller contract install
// needs. The name is historical; Linux uses systemd, macOS uses launchd.
type SystemdOps interface {
	WriteUnit(name, content string) error
	DaemonReload() error
	Start(unit string) error
}

// InstallDeps wires install.go's external effects.
type InstallDeps struct {
	Downloader Downloader
	Systemd    SystemdOps
	// RenderServiceDef returns the systemd unit file (Linux) or LaunchAgent
	// plist (macOS) body for the supplied node-launch parameters. The
	// install handler does not care which format it gets — it just hands
	// the bytes to Systemd.WriteUnit.
	RenderServiceDef func(svcctl.NodeServiceInput) string
	Platform         string // e.g. "linux-amd64"
	DefaultCfgDir    string // default node .config directory when args.config_path is empty
	UnitName         string // e.g. "quilibrium-node.service" or "com.quilscan.node"
	UnitDir          string // systemd unit dir or LaunchAgents dir; used for residue detection
	BinaryPath       string
	User             string
	// NodeLogPath is the redirect target for plist Stdout/Err on macOS.
	// Linux ignores it (journalctl handles logging).
	NodeLogPath string

	// State persistence — symmetric load/save so the handler can read whatever
	// previous run wrote, mutate selected fields, and write the full record back.
	LoadState func() (*config.State, error)
	SaveState func(*config.State) error

	// Optional: emit arbitrary backend event (used for meta_update after success).
	EmitRaw func(map[string]interface{})

	// Optional post-install hook — run after Start succeeds and before the
	// final done status. Used for the RPC config patch + node restart.
	OnInstalled func(cfgDir, version string) error
}

// Existing summarises a pre-existing node install for the install guard.
type Existing struct {
	StateConfigPath string
	BinaryPath      string
	UnitFile        string
	SystemdUnit     bool
	SystemdActive   bool
	ProcessPID      int
}

// Found returns true if any signal of a previous install was detected.
func (e Existing) Found() bool {
	return e.StateConfigPath != "" ||
		e.BinaryPath != "" ||
		e.UnitFile != "" ||
		e.SystemdUnit ||
		e.SystemdActive ||
		e.ProcessPID > 0
}

// detectExisting probes filesystem, service manager, and process table for
// any signal that a node is already installed on this server. Any one signal
// means "don't blindly reinstall".
func detectExisting(stateCfgPath, binaryPath, unitName, unitDir string) Existing {
	var e Existing
	if stateCfgPath != "" {
		if _, err := os.Stat(stateCfgPath); err == nil {
			e.StateConfigPath = stateCfgPath
		}
	}
	if _, err := os.Stat(binaryPath); err == nil {
		e.BinaryPath = binaryPath
	}
	if unitFile := unitFilePath(unitDir, unitName); unitFile != "" {
		if _, err := os.Stat(unitFile); err == nil {
			e.UnitFile = unitFile
		}
	}
	if strings.HasSuffix(unitName, ".service") {
		if out, err := exec.Command("systemctl", "list-unit-files", "--no-legend", unitName).Output(); err == nil {
			if strings.Contains(string(out), unitName) {
				e.SystemdUnit = true
			}
		}
		if exec.Command("systemctl", "is-active", "--quiet", unitName).Run() == nil {
			e.SystemdActive = true
		}
	}
	if out, err := exec.Command("pgrep", "-x", "quilibrium-node").Output(); err == nil {
		for _, line := range strings.Fields(string(out)) {
			if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
				e.ProcessPID = pid
				break
			}
		}
	}
	return e
}

func unitFilePath(unitDir, unitName string) string {
	if unitName == "" {
		return ""
	}
	if unitDir == "" {
		if strings.HasSuffix(unitName, ".service") {
			unitDir = "/etc/systemd/system"
		} else {
			home, _ := os.UserHomeDir()
			if home == "" {
				return ""
			}
			unitDir = filepath.Join(home, "Library", "LaunchAgents")
		}
	}
	return svcctl.UnitFilePath(unitDir, unitName)
}

// NewInstallHandler wires deps and returns a Handler to register with the
// dispatcher.
func NewInstallHandler(d InstallDeps) Handler {
	return func(c Command, emit Emitter) error {
		version, _ := c.Args["version"].(string)
		if version == "" {
			emit(Status{ID: c.ID, Step: "failed", Error: "missing version"})
			return fmt.Errorf("missing version")
		}

		force, _ := c.Args["force"].(bool)
		source, _ := c.Args["source"].(string) // "fresh" | "migrated" — set by caller (e.g. migrate handler)
		if source == "" {
			source = "fresh"
		}

		// Preflight environment check — fail fast with an actionable
		// message rather than wait 90s for the rpcconfig timeout.
		// Currently only gates macOS (openssl@3 dylib + node metrics port).
		emit(Status{ID: c.ID, Step: "preflight", Progress: 0.02})
		if err := preflightInstall(d.Platform); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// === Reinstall guard ===
		// Read whatever the previous run wrote so we know where the existing
		// install thinks its config_path is.
		var prevState *config.State
		if d.LoadState != nil {
			s, err := d.LoadState()
			if err == nil {
				prevState = s
			}
		}
		stateCfgPath := ""
		if prevState != nil {
			stateCfgPath = prevState.ConfigPath
		}
		existing := detectExisting(stateCfgPath, d.BinaryPath, d.UnitName, d.UnitDir)
		if existing.Found() && !force {
			details := map[string]interface{}{
				"state_config_path": existing.StateConfigPath,
				"binary_path":       existing.BinaryPath,
				"unit_file":         existing.UnitFile,
				"systemd_unit":      existing.SystemdUnit,
				"systemd_active":    existing.SystemdActive,
				"process_pid":       existing.ProcessPID,
			}
			if prevState != nil {
				details["node_version"] = prevState.NodeVersion
				details["installed_at"] = prevState.InstalledAt
				details["install_source"] = prevState.InstallSource
			}
			if d.EmitRaw != nil {
				d.EmitRaw(map[string]interface{}{
					"type":    "existing_install_detected",
					"cmd_id":  c.ID,
					"details": details,
				})
			}
			emit(Status{ID: c.ID, Step: "existing_detected", Error: "node already installed; pass force=true to overwrite"})
			return fmt.Errorf("node already installed")
		}

		// Only the migrated path consumes args["config_path"]: the user-
		// supplied .config goes through validateConfigPath, which enforces
		// shape (absolute, ends in .config, parseable config.yml) and
		// rejects control characters. Fresh installs ignore any inbound
		// config_path entirely and use the agent's locally-configured
		// ManagedConfigDir, closing the unit-file injection surface where
		// a backend-supplied path would otherwise flow into WorkingDirectory.
		var cfgDir string
		if source == "migrated" {
			override, _ := c.Args["config_path"].(string)
			cfgDir = normalizeConfigPath(override)
			if err := validateConfigPath(cfgDir); err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
			c.Args["config_path"] = cfgDir
		} else {
			cfgDir = normalizeConfigPath(d.DefaultCfgDir)
		}

		emit(Status{ID: c.ID, Step: "preparing", Progress: 0.05})
		if source != "migrated" {
			// Create only the WorkDir parent; let the node create `.config`
			// itself on first start. If we pre-create `.config` here, the
			// reconcile loop sees it during the long download window and
			// flags residue (binary missing + config dir present) — which
			// is misleading mid-install.
			if err := os.MkdirAll(filepath.Dir(cfgDir), 0o755); err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
		}
		releaseDir, cleanupReleaseDir, err := makeNodeReleaseTempDir(d.BinaryPath)
		if err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		defer cleanupReleaseDir()

		emit(Status{ID: c.ID, Step: "downloading", Progress: 0.20})
		if err := d.Downloader.Download(version, d.Platform, releaseDir); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// Quilibrium publishes binaries with version-templated filenames
		// (node-2.1.0.22-linux-amd64). The service definition needs a stable
		// binary path, so we move the binary and its digest/signature sidecars
		// to the same stable path prefix. The node looks for <binary>.dgst at
		// startup.
		emit(Status{ID: c.ID, Step: "installing_binary", Progress: 0.55})
		versionedBinary := filepath.Join(releaseDir, fmt.Sprintf("node-%s-%s", version, d.Platform))
		if err := installNodeBinary(versionedBinary, d.BinaryPath); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		emit(Status{ID: c.ID, Step: "registering", Progress: 0.80})
		// Fresh and migrated installs both rely on WorkDir + the node binary's
		// default `.config` behaviour, so relative paths inside config.yml keep
		// working and ExecStart does not need `-config`.
		ui := svcctl.NodeServiceInput{
			BinaryPath: d.BinaryPath,
			User:       d.User,
			WorkDir:    filepath.Dir(cfgDir),
			Label:      d.UnitName,
			LogPath:    d.NodeLogPath,
		}
		render := d.RenderServiceDef
		if render == nil {
			// Default renderer: systemd unit. Linux production callers can
			// rely on this, while macOS callers (cmd/agent/main.go on
			// darwin) explicitly inject launchd.RenderNodePlist instead.
			render = func(in svcctl.NodeServiceInput) string {
				return systemd.RenderNodeUnit(systemd.UnitInput{
					BinaryPath: in.BinaryPath,
					User:       in.User,
					ConfigPath: in.ConfigPath,
					WorkDir:    in.WorkDir,
				})
			}
		}
		unit := render(ui)
		if err := d.Systemd.WriteUnit(d.UnitName, unit); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if err := d.Systemd.DaemonReload(); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		emit(Status{ID: c.ID, Step: "starting", Progress: 0.95})
		if err := d.Systemd.Start(d.UnitName); err != nil {
			emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}

		// === Persist full state ===
		if d.SaveState != nil {
			now := time.Now().UTC()
			ns := &config.State{
				ConfigPath:    cfgDir,
				BinaryPath:    d.BinaryPath,
				ServiceUnit:   d.UnitName,
				NodeVersion:   version,
				InstallSource: source,
				InstalledAt:   now,
			}
			if source == "migrated" {
				ns.MigratedFrom, _ = c.Args["config_path"].(string)
			}
			if err := d.SaveState(ns); err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
		}

		if d.EmitRaw != nil {
			d.EmitRaw(map[string]interface{}{
				"type":         "meta_update",
				"has_node":     true,
				"node_version": version,
			})
		}

		if d.OnInstalled != nil && source != "migrated" {
			emit(Status{ID: c.ID, Step: "configuring_rpc", Progress: 0.98})
			if err := d.OnInstalled(cfgDir, version); err != nil {
				emit(Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
		}

		emit(Status{ID: c.ID, Step: "done", Progress: 1.0})
		return nil
	}
}

// installNodeBinary moves the freshly-downloaded versioned binary and its
// digest/signature sidecars onto the canonical BinaryPath prefix (e.g.
// /usr/local/bin/quilibrium-node, /usr/local/bin/quilibrium-node.dgst, and
// /usr/local/bin/quilibrium-node.dgst.sig.N). Falls back to copy when source
// and destination live on different filesystems (the user might have placed
// cfgDir on a separate disk for IO isolation).
//
// Caller is responsible for ensuring the existing binary, if any, is not
// currently being executed (ETXTBSY) — for installs there's no running node
// yet; for updates the caller must stop the service first.
func installNodeBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir bin dir: %w", err)
	}
	stagedSidecars, err := stageNodeSidecars(src, dst)
	if err != nil {
		return err
	}
	sidecarsCommitted := false
	defer func() {
		if !sidecarsCommitted {
			for _, sidecar := range stagedSidecars {
				_ = os.Remove(sidecar.tmp)
			}
		}
	}()

	if err := moveFile(src, dst, 0o755); err != nil {
		return err
	}
	for _, sidecar := range stagedSidecars {
		if err := os.Rename(sidecar.tmp, sidecar.final); err != nil {
			return fmt.Errorf("install sidecar %s: %w", sidecar.final, err)
		}
	}
	sidecarsCommitted = true
	return nil
}

func makeNodeReleaseTempDir(binaryPath string) (string, func(), error) {
	parent := filepath.Dir(binaryPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("mkdir binary dir: %w", err)
	}
	dir, err := os.MkdirTemp(parent, ".quilscan-node-release-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create release temp dir: %w", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

type stagedNodeSidecar struct {
	tmp   string
	final string
}

func stageNodeSidecars(src, dst string) ([]stagedNodeSidecar, error) {
	digestSrc := src + ".dgst"
	if _, err := os.Stat(digestSrc); err != nil {
		return nil, fmt.Errorf("stat digest: %w", err)
	}
	sigSrcs, err := filepath.Glob(src + ".dgst.sig.*")
	if err != nil {
		return nil, fmt.Errorf("glob signatures: %w", err)
	}
	if len(sigSrcs) == 0 {
		return nil, fmt.Errorf("no signature files found for %s", filepath.Base(src))
	}

	sources := append([]string{digestSrc}, sigSrcs...)
	staged := make([]stagedNodeSidecar, 0, len(sources))
	for _, source := range sources {
		final := dst + strings.TrimPrefix(source, src)
		tmp := final + ".tmp"
		if err := copyFile(source, tmp, 0o644); err != nil {
			for _, sidecar := range staged {
				_ = os.Remove(sidecar.tmp)
			}
			return nil, fmt.Errorf("stage sidecar %s: %w", filepath.Base(source), err)
		}
		staged = append(staged, stagedNodeSidecar{tmp: tmp, final: final})
	}
	return staged, nil
}

func moveFile(src, dst string, mode os.FileMode) error {
	if err := os.Rename(src, dst); err == nil {
		return os.Chmod(dst, mode)
	}
	// Cross-fs fallback
	if err := copyFile(src, dst+".tmp", mode); err != nil {
		return err
	}
	if err := os.Rename(dst+".tmp", dst); err != nil {
		os.Remove(dst + ".tmp")
		return fmt.Errorf("rename tmp: %w", err)
	}
	_ = os.Remove(src)
	return os.Chmod(dst, mode)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("close dst: %w", err)
	}
	return os.Chmod(dst, mode)
}

// ReleaseDownloader adapts internal/release into the Downloader interface.
type ReleaseDownloader struct{}

func (ReleaseDownloader) Download(version, platform, destDir string) error {
	return release.DownloadRelease(release.ReleaseBaseURL, version, platform, destDir)
}
