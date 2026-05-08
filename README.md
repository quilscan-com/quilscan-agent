# Quilscan Agent

Open-source remote-control agent for Quilibrium nodes. Pairs with [Quilscan](https://quilscan.com) via a locally-generated bearer token — lets you install, migrate, start, stop, and update your node from the `/node-console` page, no SSH needed.

## Why this is safe

- **Outbound only** — the agent opens a single WebSocket to Quilscan's backend; it never listens on any port.
- **9-action whitelist, hardcoded** — `install`, `migrate`, `start`, `stop`, `restart_agent`, `update_agent`, `update_node`, `cleanup_residue`, `rescan`. Everything else is rejected by the dispatcher. Grep `cmd/agent/main.go` for the registered handlers and `internal/actions/dispatcher.go` for the rejection path.
- **Key files stay off-limits** — the agent does not read key files such as `keys.yml`. It reads `config.yml` locally for RPC detection and peer-ID parsing; the contents are never transmitted.
- **Downloads are constrained** — node release downloads use the compile-time `ReleaseBaseURL` in `internal/release/download.go`; agent self-update URLs must match `DefaultAgentReleaseURLPrefix` in `internal/actions/update_agent.go`.
- **Audit log is human-readable** — every command received is appended to a flat file. You can `cat` it at any time to verify what was done and when.
- **Token is local** — generated on install via `crypto/rand`, stored `chmod 600`. Not persisted on Quilscan's servers.

## Install

| Platform | Command | Notes |
|---|---|---|
| Linux (amd64 / arm64, systemd) | `sudo curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh \| sudo bash` | Installs system-wide, requires sudo. |
| macOS (Apple Silicon) | `curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh \| bash` | User-level LaunchAgent, no sudo. |

The installer:

1. Detects your OS/arch and downloads the matching binary.
2. Generates a token, stores it with `0600` perms, prints it once to your terminal.
3. Registers a service (`quilscan-agent.service` on Linux, `com.quilscan.agent` LaunchAgent on macOS) and starts it.

Fresh node installs may patch empty local RPC listen addresses in `config.yml` so the dashboard can read local node data. Migrated/imported node configs are not modified.

Copy the token. Paste it into Quilscan's `/node-console` page to pair.

## File layout

| Item | Linux | macOS |
|---|---|---|
| Agent binary | `/usr/local/bin/quilscan-agent` | `~/.local/bin/quilscan-agent` |
| Service def | `/etc/systemd/system/quilscan-agent.service` | `~/Library/LaunchAgents/com.quilscan.agent.plist` |
| Token | `/etc/quilscan-agent/token` | `~/Library/Application Support/quilscan-agent/token` |
| State | `/etc/quilscan-agent/state.yaml` | `~/Library/Application Support/quilscan-agent/state.yaml` |
| Audit log | `/var/log/quilscan-agent.log` | `~/Library/Logs/quilscan-agent.log` |
| Node binary | `/usr/local/bin/quilibrium-node` | `~/.local/bin/quilibrium-node` |
| Node `.config` (fresh) | `/var/lib/quilscan/node/.config` | `~/Library/Application Support/quilibrium/.config` |

## First pairing

Open https://quilscan.com/node-console, paste your token, click Connect. You'll see:

- **No node detected** → pick "Fresh install" or "I already have one".
- **Node detected** → start/stop/restart/update from the panel.

When importing an existing node, provide the absolute path to its `.config` directory. The directory itself must be named `.config`, and `config.yml` must point to `keys.path: .config/keys.yml` or `key.keyManagerFile.path: .config/keys.yml`. The agent starts the service from the config parent directory so relative paths continue to work.

## Uninstall

```bash
# Linux
sudo curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash

# macOS
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | bash
```

Removes the agent binary, agent service definition, agent support directory, and agent log. **Never touches your node data.** The Quilibrium node binary, plist/unit, and `.config` directory are explicitly preserved.

For removing only the node (keeping the agent paired), use `remove-node.sh` from the same URL.

## Verify the security claims yourself

```bash
git clone https://github.com/quilscan-com/quilscan-agent.git
cd quilscan-agent

# 1. Action whitelist — should show exactly the 9 allowed actions.
grep -E '"(install|migrate|start|stop|restart_agent|update_agent|update_node|cleanup_residue|rescan)":' cmd/agent/main.go
grep -n "ErrForbidden" internal/actions/dispatcher.go

# 2. File reads — should be limited to agent config/state/token/logs,
#    release binaries, node config.yml, node-info / peer-info cache files,
#    and storage-size measurement. Key files such as keys.yml should not be opened.
grep -rn "ReadFile\|ioutil.ReadFile\|os.Open\|ReadDir\|WalkDir" cmd/ internal/ | grep -v _test
grep -rn "keys.yml" cmd/ internal/ | grep -v _test

# 3. Download URL guards
grep -n "ReleaseBaseURL" internal/release/download.go
grep -n "DefaultAgentReleaseURLPrefix" internal/actions/update_agent.go
```

If any of these audits fail, **do not trust the binary**.

## Build from source

```bash
git clone https://github.com/quilscan-com/quilscan-agent.git
cd quilscan-agent
make build
# Produces dist/quilscan-agent-{linux-amd64,linux-arm64,darwin-arm64}
```

Requires Go 1.22 or newer.

## Directory structure

```
quilscan-agent/
├── cmd/agent/main.go     # runtime entrypoint, action wiring
├── internal/
│   ├── actions/          # 9-action whitelist + handlers
│   ├── audit/            # append-only audit log
│   ├── config/           # config.yaml + state.yaml (platform-aware defaults)
│   ├── launchd/          # macOS plist renderer + launchctl wrapper
│   ├── logstream/        # node-log streaming (journalctl on Linux, tail -f on macOS)
│   ├── metrics/          # 3s host + node sampling
│   ├── netinfo/          # public IP + country (ip-api.com)
│   ├── nodeinfo/         # parses `node --node-info`
│   ├── nodeinstall/      # install detection (binary / config / process / unit)
│   ├── peerinfo/         # parses `node --peer-info`
│   ├── reconcile/        # background loops: verify / du / version-poll
│   ├── release/          # node release downloader (const URL)
│   ├── rpcconfig/        # patches node config.yml RPC listen addrs
│   ├── svcctl/           # platform-agnostic service control interface
│   ├── systemd/          # Linux unit renderer + systemctl wrapper
│   ├── token/            # crypto/rand bearer-token gen + 0600 save
│   └── ws/               # outbound WebSocket client
├── install.sh            # Linux + macOS installer
├── uninstall.sh          # Linux + macOS uninstaller
├── remove-node.sh        # remove node only (keep agent)
└── Makefile
```

## License

MIT
