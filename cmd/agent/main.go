package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quilscan-com/quilscan-agent/internal/actions"
	"github.com/quilscan-com/quilscan-agent/internal/audit"
	"github.com/quilscan-com/quilscan-agent/internal/config"
	"github.com/quilscan-com/quilscan-agent/internal/devnodeautoupdate"
	"github.com/quilscan-com/quilscan-agent/internal/download"
	"github.com/quilscan-com/quilscan-agent/internal/fdlimit"
	"github.com/quilscan-com/quilscan-agent/internal/launchd"
	"github.com/quilscan-com/quilscan-agent/internal/logstream"
	"github.com/quilscan-com/quilscan-agent/internal/metrics"
	"github.com/quilscan-com/quilscan-agent/internal/netinfo"
	"github.com/quilscan-com/quilscan-agent/internal/nodeinstall"
	"github.com/quilscan-com/quilscan-agent/internal/nodemanifest"
	"github.com/quilscan-com/quilscan-agent/internal/reconcile"
	"github.com/quilscan-com/quilscan-agent/internal/release"
	"github.com/quilscan-com/quilscan-agent/internal/rpcconfig"
	"github.com/quilscan-com/quilscan-agent/internal/svcctl"
	"github.com/quilscan-com/quilscan-agent/internal/systemd"
	"github.com/quilscan-com/quilscan-agent/internal/token"
	"github.com/quilscan-com/quilscan-agent/internal/ws"
)

var version = "1.1.5"

type startStopCtl interface {
	Start(string) error
	Stop(string) error
	Disable(string) error
	Reload() error
	IsActive(string) bool
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version":
			fmt.Println(version)
			return
		case "init-token":
			bootstrapToken()
			return
		}
	}
	run()
}

// bootstrapToken is invoked by install.sh once during install to provision the
// token. It prints the token to stdout then exits. The parent directory is
// derived from defaults.TokenPath so platform-specific layouts work without
// special-casing here:
//
//	Linux : /etc/quilscan-agent/token  → mkdir /etc/quilscan-agent
//	macOS : /Library/Application Support/quilscan-agent/token in system mode,
//	        or the legacy user support directory when running user-mode.
func bootstrapToken() {
	defaults := config.DefaultConfig()
	tok, err := token.Generate()
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(defaults.TokenPath), 0o755); err != nil {
		log.Fatalf("mkdir: %v", err)
	}
	if err := token.Save(defaults.TokenPath, tok); err != nil {
		log.Fatalf("save token: %v", err)
	}
	fmt.Println(tok)
}

func run() {
	defaults := config.DefaultConfig()
	cfg, err := config.Load(defaults.ConfigPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	defaults = cfg
	if err := download.ConfigureProxy(defaults.EffectiveDownloadProxyURL()); err != nil {
		log.Fatalf("download proxy: %v", err)
	}
	if proxyURL := download.ConfiguredProxyURL(); proxyURL != "" {
		log.Printf("[download] using proxy %s", proxyURL)
	}

	tok, err := token.Load(defaults.TokenPath)
	if err != nil {
		log.Fatalf("read token (run `quilscan-agent init-token` first): %v", err)
	}

	if _, err := config.LoadState(defaults.StatePath); err != nil {
		log.Fatalf("state: %v", err)
	}

	auditor, err := audit.New(defaults.AuditLogPath)
	if err != nil {
		log.Printf("audit disabled: %v", err)
	}

	platform := release.DetectPlatform(runtime.GOOS, runtime.GOARCH)
	if platform == "" {
		log.Fatalf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigs; cancel() }()

	startedAt := time.Now()
	agentServiceMode, nodeServiceMode := serviceModes(cfg)

	// Create client first so installDeps callbacks can close over it.
	client := &ws.Client{
		URL:   cfg.BackendURL,
		Token: tok,
		Meta: ws.Meta{
			Version:         version,
			OS:              platform,
			HasNode:         hasExistingNode(defaults),
			HasQClient:      hasExistingQClient(defaults),
			ServiceMode:     agentServiceMode,
			NodeServiceMode: nodeServiceMode,
		},
	}

	// Service-manager backend: systemd on Linux, launchd on macOS. The
	// FSOps + Ctl pair handles writing service definitions to disk
	// (systemd unit / launchd plist) and lifecycle commands. defaults is
	// already platform-aware (DefaultConfig branches on runtime.GOOS).
	var sdOps actions.SystemdOps
	var sdCtl startStopCtl
	var renderNodeDef func(svcctl.NodeServiceInput) string
	if runtime.GOOS == "darwin" {
		sdOps = launchd.FSOps{UnitDir: defaults.UnitDir}
		sdCtl = launchd.Ctl{UnitDir: defaults.UnitDir}
		renderNodeDef = launchd.RenderNodePlist
	} else {
		sdOps = systemd.FSOps{UnitDir: defaults.UnitDir}
		sdCtl = systemd.Ctl{}
		renderNodeDef = func(in svcctl.NodeServiceInput) string {
			return systemd.RenderNodeUnit(systemd.UnitInput{
				BinaryPath: in.BinaryPath,
				User:       in.User,
				ConfigPath: in.ConfigPath,
				WorkDir:    in.WorkDir,
			})
		}
	}
	// Post-install hook: wait for node's auto-generated config.yml, set
	// loopback listen multiaddrs (without overriding user values), restart
	// the service so the agent can talk to RPC. Persists rpc_patched flag.
	onInstalled := func(cfgDir, ver string) error {
		log.Printf("[rpcconfig] waiting for %s/config.yml", cfgDir)
		res, err := rpcconfig.WaitAndPatch(sdCtl, defaults.NodeServiceName, cfgDir, 90*time.Second)
		if err != nil {
			log.Printf("[rpcconfig] %v", err)
			_ = client.Send(map[string]interface{}{
				"type":  "rpc_patch_failed",
				"error": err.Error(),
			})
			return err
		}
		log.Printf("[rpcconfig] grpc=%s rest=%s restarted=%v",
			res.GRPCFinalValue, res.RESTFinalValue, res.Restarted)
		// Persist patched ports
		s, _ := config.LoadState(defaults.StatePath)
		if s != nil {
			s.RPCPatched = true
			s.RPCGRPCPort = rpcconfig.GRPCPort
			s.RPCRESTPort = rpcconfig.RESTPort
			s.RPCPatchedAt = time.Now().UTC()
			_ = config.SaveState(defaults.StatePath, s)
		}
		_ = client.Send(map[string]interface{}{
			"type":           "rpc_patched",
			"grpc_multiaddr": res.GRPCFinalValue,
			"rest_multiaddr": res.RESTFinalValue,
			"restarted":      res.Restarted,
		})
		return nil
	}

	// Forward-declared so update_node's PatchNodeStatus closure can capture it
	// without a circular construction (handler is built before the loop value
	// is assigned, but only invoked once the loop is already running).
	var rec *reconcile.Loop

	installUser := "root"
	if runtime.GOOS == "darwin" {
		installUser = "" // launchd runs jobs as the user that bootstrapped them
	}
	qclientReleaseURL := strings.TrimSpace(os.Getenv("QUILSCAN_QCLIENT_RELEASE_URL"))
	if qclientReleaseURL == "" {
		qclientReleaseURL = defaults.QClientReleaseURL
	}
	qclientLatestVersionURL := ""
	if qclientReleaseURL != "" {
		qclientLatestVersionURL = strings.TrimRight(qclientReleaseURL, "/") + "/" + release.QClientVersionManifest
	}
	qclientInstallDeps := func() actions.QClientInstallDeps {
		return actions.QClientInstallDeps{
			BinaryPath: defaults.QClientBinaryPath,
			Platform:   platform,
			Downloader: actions.QClientReleaseDownloader{BaseURL: qclientReleaseURL},
			LoadState:  func() (*config.State, error) { return config.LoadState(defaults.StatePath) },
			SaveState:  func(s *config.State) error { return config.SaveState(defaults.StatePath, s) },
			EmitRaw:    func(m map[string]interface{}) { _ = client.Send(m) },
			PatchNodeStatus: func(patch map[string]interface{}) {
				if rec != nil {
					rec.PatchNodeStatus(patch)
				}
			},
		}
	}
	var qclientInstallMu sync.Mutex
	installQClient := func() (string, error) {
		qclientInstallMu.Lock()
		defer qclientInstallMu.Unlock()
		version, err := actions.EnsureQClientCurrent(qclientInstallDeps())
		if err == nil {
			client.Meta.HasQClient = true
		}
		return version, err
	}
	go ensureQClientInstalledOnStartup(defaults, installQClient)
	installHandler := actions.NewInstallHandler(actions.InstallDeps{
		Downloader:       actions.ReleaseDownloader{},
		DevInstaller:     actions.ManifestDevNodeInstaller{},
		NodeManifestURL:  nodemanifest.DefaultURL,
		Systemd:          sdOps,
		RenderServiceDef: renderNodeDef,
		Platform:         platform,
		DefaultCfgDir:    defaults.ManagedConfigDir,
		UnitName:         defaults.NodeServiceName,
		UnitDir:          defaults.UnitDir,
		BinaryPath:       defaults.NodeBinaryPath,
		User:             installUser,
		NodeLogPath:      defaults.NodeLogPath,
		LoadState:        func() (*config.State, error) { return config.LoadState(defaults.StatePath) },
		SaveState:        func(s *config.State) error { return config.SaveState(defaults.StatePath, s) },
		EmitRaw:          func(m map[string]interface{}) { _ = client.Send(m) },
		OnInstalled:      onInstalled,
		InstallQClient:   installQClient,
	})
	nodeUpdateGate := &actions.NodeUpdateGate{}
	nodeUpdateHandler := actions.NewUpdateNodeHandler(actions.NodeUpdaterDeps{
		UnitName:                          defaults.NodeServiceName,
		BinaryPath:                        defaults.NodeBinaryPath,
		Platform:                          platform,
		StartStop:                         sdCtl,
		Downloader:                        actions.ReleaseDownloader{},
		DevInstaller:                      actions.ManifestDevNodeInstaller{},
		NodeManifestURL:                   nodemanifest.DefaultURL,
		Gate:                              nodeUpdateGate,
		SuppressAutomaticContentionStatus: true,
		LoadState:                         func() (*config.State, error) { return config.LoadState(defaults.StatePath) },
		SaveState:                         func(s *config.State) error { return config.SaveState(defaults.StatePath, s) },
		EmitRaw:                           func(m map[string]interface{}) { _ = client.Send(m) },
		PatchNodeStatus: func(patch map[string]interface{}) {
			if rec != nil {
				rec.PatchNodeStatus(patch)
			}
		},
	})
	devAutoController := devnodeautoupdate.NewController(devnodeautoupdate.Deps{
		StatePath:   defaults.StatePath,
		Update:      nodeUpdateHandler,
		UpdateOwner: nodeUpdateGate.Owner,
		PatchNodeStatus: func(patch map[string]interface{}) {
			if rec != nil {
				rec.PatchNodeStatus(patch)
			}
		},
		EmitCommandStatus: func(s actions.Status) {
			_ = client.Send(statusMessage(s))
		},
	})

	d := &actions.Dispatcher{
		Handlers: map[string]actions.Handler{
			"install": installHandler,
			"migrate": actions.NewMigrateHandler(actions.MigrateDeps{Install: installHandler}),
			"start":   actions.NewStartHandler(sdCtl, defaults.NodeServiceName),
			"stop":    actions.NewStopHandler(sdCtl, defaults.NodeServiceName),
			"rescan": actions.NewRescanHandler(func() bool {
				if rec == nil {
					return false
				}
				return rec.RunVerifyNow()
			}),
			"refresh_qclient_allocations": actions.NewRefreshQClientAllocationsHandler(actions.RefreshQClientAllocationsDeps{
				StatePath:         defaults.StatePath,
				QClientBinaryPath: defaults.QClientBinaryPath,
				ManagedConfigDir:  defaults.ManagedConfigDir,
				PatchNodeStatus: func(patch map[string]interface{}) {
					if rec != nil {
						rec.PatchNodeStatus(patch)
					}
				},
			}),
			"check_public_stream_ports": actions.NewCheckPublicStreamPortsHandler(actions.CheckPublicStreamPortsDeps{
				StatePath:        defaults.StatePath,
				ManagedConfigDir: defaults.ManagedConfigDir,
			}),
			"qclient_manage_action": actions.NewQClientManageActionHandler(actions.QClientManageActionDeps{
				StatePath:         defaults.StatePath,
				QClientBinaryPath: defaults.QClientBinaryPath,
				ManagedConfigDir:  defaults.ManagedConfigDir,
				PatchNodeStatus: func(patch map[string]interface{}) {
					if rec != nil {
						rec.PatchNodeStatus(patch)
					}
				},
			}),
			"update_agent": actions.NewUpdateAgentHandler(actions.AgentUpdaterDeps{
				AgentBinaryPath: defaults.AgentBinaryPath,
				Platform:        platform,
				SelfServiceUnit: defaults.AgentServiceName,
				Svc:             svcctl.New(),
			}),
			"restart_agent": actions.NewRestartAgentHandler(actions.RestartAgentDeps{
				SelfServiceUnit: defaults.AgentServiceName,
				Svc:             svcctl.New(),
			}),
			"cleanup_residue": actions.NewCleanupResidueHandler(actions.CleanupDeps{
				StatePath:         defaults.StatePath,
				ManagedConfigDir:  defaults.ManagedConfigDir,
				BinaryPath:        defaults.NodeBinaryPath,
				QClientBinaryPath: defaults.QClientBinaryPath,
				UnitName:          defaults.NodeServiceName,
				BackupRootDir:     defaults.BackupRootDir,
				UnitDir:           defaults.UnitDir,
				Systemd:           sdCtl,
				EmitRaw:           func(m map[string]interface{}) { _ = client.Send(m) },
				PatchNodeStatus: func(patch map[string]interface{}) {
					if rec != nil {
						rec.PatchNodeStatus(patch)
					}
				},
			}),
			"delete_node_store": actions.NewDeleteNodeStoreHandler(actions.DeleteNodeStoreDeps{
				StatePath:     defaults.StatePath,
				BackupRootDir: defaults.BackupRootDir,
				UnitName:      defaults.NodeServiceName,
				Systemd:       sdCtl,
				EmitRaw:       func(m map[string]interface{}) { _ = client.Send(m) },
				PatchNodeStatus: func(patch map[string]interface{}) {
					if rec != nil {
						rec.PatchNodeStatus(patch)
					}
				},
			}),
			"delete_node_store_backup": actions.NewDeleteNodeStoreBackupHandler(actions.DeleteNodeStoreBackupDeps{
				BackupRootDir: defaults.BackupRootDir,
				EmitRaw:       func(m map[string]interface{}) { _ = client.Send(m) },
				PatchNodeStatus: func(patch map[string]interface{}) {
					if rec != nil {
						rec.PatchNodeStatus(patch)
					}
				},
			}),
			"update_node":              nodeUpdateHandler,
			"set_dev_node_auto_update": devnodeautoupdate.NewSetEnabledHandler(devAutoController),
			"switch_node_source": actions.NewSwitchNodeSourceHandler(actions.NodeSourceSwitcherDeps{
				UnitName:        defaults.NodeServiceName,
				UnitDir:         defaults.UnitDir,
				BinaryPath:      defaults.NodeBinaryPath,
				Platform:        platform,
				StartStop:       sdCtl,
				Reload:          sdCtl.Reload,
				Downloader:      actions.ReleaseDownloader{},
				DevInstaller:    actions.ManifestDevNodeInstaller{},
				NodeManifestURL: nodemanifest.DefaultURL,
				LoadState:       func() (*config.State, error) { return config.LoadState(defaults.StatePath) },
				SaveState:       func(s *config.State) error { return config.SaveState(defaults.StatePath, s) },
				EmitRaw:         func(m map[string]interface{}) { _ = client.Send(m) },
				PatchNodeStatus: func(patch map[string]interface{}) {
					if rec != nil {
						rec.PatchNodeStatus(patch)
					}
				},
			}),
			"repair_fd_limit": newRepairFDLimitHandler(defaults, sdCtl, func() bool {
				if rec == nil {
					return false
				}
				return rec.RunVerifyNow()
			}),
			"install_qclient": actions.NewInstallQClientHandler(qclientInstallDeps()),
		},
	}

	var collector *metrics.Collector
	logStreamer := &logstream.Streamer{
		UnitName: defaults.NodeServiceName,
		LogPath:  defaults.NodeLogPath, // empty on Linux → journalctl path
	}
	client.OnMessage = func(b []byte) {
		var env struct {
			Type   string `json:"type"`
			Target string `json:"target"`
			Lines  int    `json:"lines"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			log.Printf("[control] ignored invalid frame: %v", err)
			return
		}
		if env.Type == "stream_on" {
			log.Printf("[control] stream_on")
			collector.SetStreaming(true)
			return
		}
		if env.Type == "stream_off" {
			log.Printf("[control] stream_off")
			collector.SetStreaming(false)
			return
		}
		if env.Type == "logs_on" {
			log.Printf("[control] logs_on target=%s lines=%d", env.Target, env.Lines)
			if err := logStreamer.Start(ctx, env.Target, env.Lines, func(m map[string]interface{}) { _ = client.Send(m) }); err != nil {
				log.Printf("[control] logs_on failed target=%s: %v", env.Target, err)
				_ = client.Send(map[string]interface{}{
					"type":   "logs",
					"target": env.Target,
					"error":  err.Error(),
					"lines":  []interface{}{},
				})
			}
			return
		}
		if env.Type == "logs_off" {
			log.Printf("[control] logs_off")
			logStreamer.Stop()
			return
		}
		if env.Type != "cmd" {
			log.Printf("[control] ignored type=%s", env.Type)
			return
		}
		var cmd actions.Command
		if err := json.Unmarshal(b, &cmd); err != nil {
			log.Printf("[cmd] invalid command frame: %v", err)
			return
		}
		log.Printf("[cmd] received id=%s action=%s", cmd.ID, cmd.Action)
		err := d.Dispatch(cmd, func(s actions.Status) {
			s.ID = cmd.ID
			s.Action = cmd.Action
			_ = client.Send(statusMessage(s))
		})
		if err != nil {
			log.Printf("[cmd] finished id=%s action=%s result=failure error=%v", cmd.ID, cmd.Action, err)
		} else {
			log.Printf("[cmd] finished id=%s action=%s result=success", cmd.ID, cmd.Action)
		}
		if auditor != nil {
			res := "success"
			if err != nil {
				res = "failure"
			}
			_ = auditor.Record(cmd.Action, res, map[string]string{"id": cmd.ID})
		}
	}

	// Background workers — independent of WS connection lifecycle, so they
	// keep producing data even during reconnect storms.
	collector = &metrics.Collector{
		Sender:   client,
		Tick:     3 * time.Second,
		IdleTick: 60 * time.Second,
		Started:  startedAt,
		DiskPath: "/",
		UnitName: defaults.NodeServiceName,
		Svc:      svcctl.New(),
	}
	go collector.Run(ctx)

	// Plug the platform-aware service controller into the reconcile loop so
	// IsActive / StartedAt go through launchctl on macOS instead of failing
	// silently against a missing systemctl.
	var reconcileSvc reconcile.ServiceCtl
	if runtime.GOOS == "darwin" {
		reconcileSvc = svcctl.New()
	} else {
		reconcileSvc = svcctl.New()
	}
	rec = &reconcile.Loop{
		StatePath:            defaults.StatePath,
		UnitName:             defaults.NodeServiceName,
		BinaryPath:           defaults.NodeBinaryPath,
		QClientBinaryPath:    defaults.QClientBinaryPath,
		ManagedConfigDir:     defaults.ManagedConfigDir,
		UnitDir:              defaults.UnitDir,
		Platform:             platform,
		Sender:               client,
		AgentBinaryPath:      defaults.AgentBinaryPath,
		AgentTokenPath:       defaults.TokenPath,
		AgentConfigYAMLPath:  defaults.ConfigPath,
		AgentAuditLogPath:    defaults.AuditLogPath,
		AgentServiceName:     defaults.AgentServiceName,
		ServiceMode:          agentServiceMode,
		NodeServiceMode:      nodeServiceMode,
		NodeLogPath:          defaults.NodeLogPath,
		Svc:                  reconcileSvc,
		NodeManifestURL:      nodemanifest.DefaultURL,
		OfficialArtifactsURL: nodemanifest.OfficialArtifactsURLFromBackend(cfg.BackendURL),
	}
	if qclientLatestVersionURL != "" {
		rec.LatestQClientVersionURL = qclientLatestVersionURL
	}
	go rec.Run(ctx)
	go devAutoController.Run(ctx)

	// netinfo: resolve public IP + country once at startup and every 24h, then
	// re-send on each WS reconnect via OnConnected so the backend's per-token
	// MetaSnapshot stays populated across drops.
	var netMu sync.Mutex
	var netCache netinfo.Info
	sendNetinfo := func() {
		netMu.Lock()
		info := netCache
		netMu.Unlock()
		if info.PublicIP == "" {
			return
		}
		_ = client.Send(map[string]interface{}{
			"type":         "meta_update",
			"public_ip":    info.PublicIP,
			"country":      info.Country,
			"country_code": info.CountryCode,
		})
	}
	client.OnConnected = sendNetinfo
	go func() {
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			lookupCtx, cancelLookup := context.WithTimeout(ctx, 15*time.Second)
			info, err := netinfo.Lookup(lookupCtx)
			cancelLookup()
			if err != nil {
				log.Printf("[netinfo] lookup failed: %v", err)
			} else {
				log.Printf("[netinfo] %s - %s (%s)", info.PublicIP, info.Country, info.CountryCode)
				netMu.Lock()
				changed := info != netCache
				netCache = info
				netMu.Unlock()
				if changed {
					sendNetinfo()
				}
			}
			select {
			case <-t.C:
			case <-ctx.Done():
				return
			}
		}
	}()

	log.Printf("quilscan-agent %s: connecting to %s", version, cfg.BackendURL)
	client.Run(ctx)
}

// hasExistingNode reports whether the agent should consider a node installed
// for the auth handshake. A node is managed only when both the stable binary
// and the recorded config directory exist; binary-only or config/unit/process
// leftovers are reported as residue by reconcile.
func hasExistingNode(defaults config.Config) bool {
	state, _ := config.LoadState(defaults.StatePath)
	recordedConfig := ""
	if state != nil {
		recordedConfig = state.ConfigPath
	}
	return hasExistingNodeAt(
		defaults.NodeBinaryPath,
		recordedConfig,
		defaults.ManagedConfigDir,
		filepath.Join(defaults.UnitDir, defaults.NodeServiceName),
	)
}

func hasExistingQClient(defaults config.Config) bool {
	if defaults.QClientBinaryPath == "" {
		return false
	}
	st, err := os.Stat(defaults.QClientBinaryPath)
	return err == nil && !st.IsDir()
}

func serviceModes(cfg config.Config) (string, string) {
	agentMode := normalizeServiceMode(cfg.ServiceMode)
	if agentMode == "" {
		agentMode = inferServiceMode(cfg.UnitDir)
	}
	nodeMode := normalizeServiceMode(cfg.NodeServiceMode)
	if nodeMode == "" {
		nodeMode = agentMode
	}
	return agentMode, nodeMode
}

func normalizeServiceMode(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "root", "daemon", "launchdaemon":
		return "system"
	case "launchagent":
		return "user"
	default:
		return v
	}
}

func inferServiceMode(unitDir string) string {
	if runtime.GOOS != "darwin" {
		return "system"
	}
	clean := filepath.Clean(unitDir)
	if clean == "/Library/LaunchDaemons" {
		return "system"
	}
	return "user"
}

func ensureQClientInstalledOnStartup(defaults config.Config, installQClient func() (string, error)) {
	if hasExistingQClient(defaults) || installQClient == nil {
		return
	}
	if !hasRecordedNode(defaults.StatePath, defaults.NodeBinaryPath) {
		log.Printf("[qclient] missing at %s; no recorded node yet, skipping startup install", defaults.QClientBinaryPath)
		return
	}
	log.Printf("[qclient] missing at %s; installing latest qclient", defaults.QClientBinaryPath)
	version, err := installQClient()
	if err != nil {
		log.Printf("[qclient] auto install failed: %v", err)
		return
	}
	if version != "" {
		log.Printf("[qclient] auto installed v%s", version)
	} else {
		log.Printf("[qclient] auto install skipped; qclient already present")
	}
}

func hasRecordedNode(statePath, binaryPath string) bool {
	state, err := config.LoadState(statePath)
	if err != nil || state == nil || state.ConfigPath == "" {
		return false
	}
	nodeBinaryPath := state.BinaryPath
	if nodeBinaryPath == "" {
		nodeBinaryPath = binaryPath
	}
	if st, err := os.Stat(nodeBinaryPath); err != nil || st.IsDir() {
		return false
	}
	st, err := os.Stat(state.ConfigPath)
	return err == nil && st.IsDir()
}

func ensureNodeLaunchdFileLimit(defaults config.Config) (bool, error) {
	if defaults.NodeBinaryPath == "" || defaults.UnitDir == "" || defaults.NodeServiceName == "" {
		return false, nil
	}
	if st, err := os.Stat(defaults.NodeBinaryPath); err != nil || st.IsDir() {
		return false, nil
	}
	plistPath := filepath.Join(defaults.UnitDir, defaults.NodeServiceName+".plist")
	if st, err := os.Stat(plistPath); err != nil || st.IsDir() {
		return false, nil
	}
	changed := false
	if filepath.Clean(defaults.UnitDir) == "/Library/LaunchDaemons" {
		globalChanged, err := launchd.EnsureSystemFileLimit(launchd.NodeFileLimit)
		if err != nil {
			return changed, err
		}
		changed = changed || globalChanged
	}
	plistChanged, err := launchd.EnsureNodeFileLimit(plistPath, launchd.NodeFileLimit)
	if err != nil {
		return changed, err
	}
	changed = changed || plistChanged
	return changed, nil
}

func ensureNodeSystemdFileLimit(defaults config.Config, ctl startStopCtl) (bool, error) {
	if ctl == nil || defaults.NodeBinaryPath == "" || defaults.UnitDir == "" || defaults.NodeServiceName == "" {
		return false, nil
	}
	if st, err := os.Stat(defaults.NodeBinaryPath); err != nil || st.IsDir() {
		return false, nil
	}
	unitPath := filepath.Join(defaults.UnitDir, defaults.NodeServiceName)
	if st, err := os.Stat(unitPath); err != nil || st.IsDir() {
		return false, nil
	}
	changed, err := systemd.EnsureNodeFileLimit(unitPath, systemd.NodeFileLimit)
	if err != nil || !changed {
		return changed, err
	}
	return true, ctl.Reload()
}

func newRepairFDLimitHandler(defaults config.Config, ctl startStopCtl, refresh func() bool) actions.Handler {
	return func(c actions.Command, emit actions.Emitter) error {
		emit(actions.Status{ID: c.ID, Step: "checking", Progress: 0.15})
		wasActive := ctl != nil && ctl.IsActive(defaults.NodeServiceName)
		restartRequired := nodeFileLimitRestartRequired(defaults)
		changed, err := repairNodeFileLimit(defaults, ctl)
		if err != nil {
			emit(actions.Status{ID: c.ID, Step: "failed", Error: err.Error()})
			return err
		}
		if wasActive || changed || restartRequired {
			emit(actions.Status{ID: c.ID, Step: "stopping", Progress: 0.45})
			if err := ctl.Stop(defaults.NodeServiceName); err != nil {
				emit(actions.Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
			emit(actions.Status{ID: c.ID, Step: "starting", Progress: 0.70})
			if err := ctl.Start(defaults.NodeServiceName); err != nil {
				emit(actions.Status{ID: c.ID, Step: "failed", Error: err.Error()})
				return err
			}
		}
		emit(actions.Status{ID: c.ID, Step: "refreshing", Progress: 0.90})
		if refresh != nil {
			refresh()
		}
		message := "file descriptor limit already matches the recommended value"
		if changed || restartRequired {
			message = "file descriptor limit updated and node restarted"
		} else if wasActive {
			message = "file descriptor limit already matched the recommended value; node restarted"
		}
		emit(actions.Status{ID: c.ID, Step: "done", Progress: 1.0, Message: message})
		return nil
	}
}

func nodeFileLimitRestartRequired(defaults config.Config) bool {
	if defaults.NodeBinaryPath == "" || defaults.UnitDir == "" || defaults.NodeServiceName == "" {
		return false
	}
	if st, err := os.Stat(defaults.NodeBinaryPath); err != nil || st.IsDir() {
		return false
	}
	unitName := defaults.NodeServiceName
	unitPath := filepath.Join(defaults.UnitDir, unitName)
	if runtime.GOOS == "darwin" {
		if !strings.HasSuffix(unitName, ".plist") {
			unitPath = filepath.Join(defaults.UnitDir, unitName+".plist")
		}
	}
	if st, err := os.Stat(unitPath); err != nil || st.IsDir() {
		return false
	}
	status := fdlimit.Inspect(fdlimit.InspectRequest{
		Platform: runtime.GOOS,
		UnitDir:  defaults.UnitDir,
		UnitName: defaults.NodeServiceName,
	})
	return status.RestartRequired && status.Status != fdlimit.StatusDisabled && status.Status != fdlimit.StatusError
}

func repairNodeFileLimit(defaults config.Config, ctl startStopCtl) (bool, error) {
	switch runtime.GOOS {
	case "darwin":
		return ensureNodeLaunchdFileLimit(defaults)
	case "linux":
		return ensureNodeSystemdFileLimit(defaults, ctl)
	default:
		return false, fmt.Errorf("file descriptor limit repair is unsupported on %s", runtime.GOOS)
	}
}

func hasExistingNodeAt(binaryPath, stateCfgPath, defaultCfgDir, unitFilePath string) bool {
	_ = unitFilePath
	return nodeinstall.Detect(nodeinstall.Paths{
		BinaryPath:        binaryPath,
		ManagedConfigDir:  defaultCfgDir,
		RecordedConfigDir: stateCfgPath,
	}).HasNode
}

func statusMessage(s actions.Status) map[string]interface{} {
	return map[string]interface{}{
		"type":     "cmd_status",
		"id":       s.ID,
		"action":   s.Action,
		"step":     s.Step,
		"progress": s.Progress,
		"error":    s.Error,
		"message":  s.Message,
	}
}
