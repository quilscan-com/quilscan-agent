# Quilscan Agent

Open-source remote-control agent for Quilibrium nodes. Pairs with [Quilscan](https://quilscan.com) via a locally-generated bearer token — lets you install, migrate, start, stop, and update your node from the `/node-console` page, no SSH needed.

## Why this is safe

- **Outbound only** — the agent opens a single WebSocket to Quilscan's backend; it never listens on any port.
- **Action whitelist, hardcoded** — `install`, `migrate`, `start`, `stop`, `rescan`, `refresh_qclient_allocations`, `check_public_stream_ports`, `qclient_manage_action`, `restart_agent`, `update_agent`, `update_node`, `switch_node_source`, `cleanup_residue`, `delete_node_store`, `delete_node_store_backup`, and `install_qclient`. Everything else is rejected by the dispatcher. Grep `cmd/agent/main.go` for the registered handlers and `internal/actions/dispatcher.go` for the rejection path.
- **Key files stay off-limits** — the agent does not read key files such as `keys.yml`. It reads `config.yml` locally for RPC detection and peer-ID parsing; the contents are never transmitted.
- **Runtime downloads are constrained and verified where applicable** — node release downloads use the compile-time `ReleaseBaseURL` in `internal/release/download.go`; agent self-update URLs must match `DefaultAgentReleaseURLPrefix` in `internal/actions/update_agent.go` and pass the built-in Ed25519 signature check before replacement. The bootstrap install and macOS migration shell scripts trust HTTPS plus the release bucket.
- **Audit log is human-readable** — every command received is appended to a flat file. You can `cat` it at any time to verify what was done and when.
- **Token is local** — generated on install via `crypto/rand`, stored `chmod 600`. Not persisted on Quilscan's servers.

## Install

| Platform | Command | Notes |
|---|---|---|
| Linux (amd64 / arm64, systemd) | `curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh \| sudo bash` | Installs system-wide, requires sudo. |
| macOS (Apple Silicon) | `curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh \| sudo bash` | System LaunchDaemon, requires sudo so it survives lock/logout. |

The installer:

1. Detects your OS/arch and downloads the matching binary.
2. Generates a token, stores it with `0600` perms, prints it once to your terminal.
3. Registers a service (`quilscan-agent.service` on Linux, `com.quilscan.agent` LaunchDaemon on macOS) and starts it.

Fresh node installs may patch empty local RPC listen addresses in `config.yml` so the dashboard can read local node data. Migrated/imported node configs are not modified.

Copy the token. Paste it into Quilscan's `/node-console` page to pair.

## macOS user-mode migration

Older macOS installs used a per-user LaunchAgent under `~/Library/LaunchAgents`
and binaries under `~/.local/bin`. Current macOS installs use root-owned system
LaunchDaemons so the agent and managed node survive lock/logout.

If the installer detects a legacy user-mode agent, run:

```bash
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/migrate-macos-root.sh | sudo bash -s -- --yes
```

The migration script:

1. Downloads the latest macOS agent from the release bucket.
2. Moves the agent, and any managed node/qclient service + binaries, to system paths.
3. Backs up old user-mode agent/node/qclient LaunchAgents and binaries under `/Library/Application Support/quilscan-agent/backups/`.
4. Leaves existing node `.config` directories, keys, worker store, and data in place.

If only a Quilibrium node is present and no Quilscan agent token/state exists,
the script does not adopt that node automatically. Install the agent first, then
use "Migrate Existing" from the dashboard.

## Agent self-update verification

Agent self-updates are verified before the running binary is replaced.

For each platform release, publish the binary and its detached signature in the
agent release bucket:

```text
quilscan-agent-linux-amd64
quilscan-agent-linux-amd64.sig
quilscan-agent-linux-arm64
quilscan-agent-linux-arm64.sig
quilscan-agent-darwin-arm64
quilscan-agent-darwin-arm64.sig
```

The update action only accepts URLs under `DefaultAgentReleaseURLPrefix`, which
defaults to:

```text
https://qstorage.quilibrium.com/quilscan-agent
```

During an update, the agent downloads the selected platform binary to a
temporary `.new` path, downloads the signature from the same URL plus `.sig`,
and verifies:

```text
ed25519.Verify(built_in_public_key, binary_bytes, base64_signature)
```

Only after verification succeeds does the agent make the file executable,
replace the installed binary, and restart the agent service. If the signature is
missing, malformed, or does not match the downloaded bytes, the update fails and
the existing agent binary is left in place.

## Dev Node public builds

Quilscan Dev Node binaries are separate from official Quilibrium release
artifacts. They are built in a public repository so users can inspect the
upstream source commit, workflow definition, build logs, published SHA-256, and
detached signature for each platform.

The public build artifacts are meant to be independently checkable:

1. The workflow records the upstream Quilibrium commit used for the build.
2. The build runs publicly for `linux-amd64` and `darwin-arm64`.
3. Each platform artifact includes the node binary, `SHA256SUMS`,
   `build-info.json`, and a detached `.sig` signature.
4. Users can compare the downloaded binary SHA-256 with the published checksum
   and inspect the workflow logs to reproduce the build path.
5. The agent verifies the detached signature with a public key pinned into the
   agent binary before installing a Dev Node artifact.

When installing, switching to, or updating a Dev Node, the agent reads
`node-version.json`, downloads the platform binary and `signature_url`, verifies
the binary bytes with the built-in public key, checks the SHA-256 against the
manifest, then replaces only the managed node binary. Config files, keys, peer
ID, worker store, and node data are not replaced.

Unsigned legacy Dev Node entries can still be identified by SHA-256, but new
Dev Node installs and updates require a signed artifact. If the manifest entry
does not include `signature_url`, or the signature object is missing, the agent
refuses the Dev Node replacement instead of silently installing an unsigned
binary.

## File layout

| Item | Linux | macOS |
|---|---|---|
| Agent binary | `/usr/local/bin/quilscan-agent` | `/usr/local/bin/quilscan-agent` |
| Service def | `/etc/systemd/system/quilscan-agent.service` | `/Library/LaunchDaemons/com.quilscan.agent.plist` |
| Token | `/etc/quilscan-agent/token` | `/Library/Application Support/quilscan-agent/token` |
| State | `/etc/quilscan-agent/state.yaml` | `/Library/Application Support/quilscan-agent/state.yaml` |
| Audit log | `/var/log/quilscan-agent.log` | `/Library/Logs/quilscan-agent.log` |
| Node binary | `/usr/local/bin/quilibrium-node` | `/usr/local/bin/quilibrium-node` |
| Node `.config` (fresh) | `/var/lib/quilscan/node/.config` | `/Library/Application Support/quilibrium/.config` |

`state.yaml` is created when node/qclient state is first persisted. A fresh
agent-only install can have `config.yaml` and `token` without a `state.yaml`
until a node is installed or imported.

## First pairing

Open https://quilscan.com/node-console, paste your token, click Connect. You'll see:

- **No node detected** → pick "Fresh install" or "I already have one".
- **Node detected** → start/stop/restart/update from the panel.

When importing an existing node, provide the absolute path to its `.config` directory. The directory itself must be named `.config`, and `config.yml` must point to `keys.path: .config/keys.yml` or `key.keyManagerFile.path: .config/keys.yml`. The agent starts the service from the config parent directory so relative paths continue to work.

## Uninstall

```bash
# Linux
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash

# macOS
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash
```

Removes the agent binary, agent service definition, agent support directory, and agent log. **Never touches your node data.** The Quilibrium node binary, plist/unit, and `.config` directory are explicitly preserved.

For removing only the node (keeping the agent paired), use `remove-node.sh` from the same URL.

## Verify the security claims yourself

```bash
git clone https://github.com/quilscan-com/quilscan-agent.git
cd quilscan-agent

# 1. Action whitelist — should show every registered action.
sed -n '/Handlers: map\[string\]actions.Handler{/,/var collector/p' cmd/agent/main.go \
  | grep -n '^[[:space:]]*"[a-z_]*":'
grep -n "ErrForbidden" internal/actions/dispatcher.go

# 2. File reads — should be limited to agent config/state/token/logs,
#    release binaries, node config.yml, node-info / peer-info cache files,
#    and storage-size measurement. Key files such as keys.yml should not be opened.
grep -rn "ReadFile\|ioutil.ReadFile\|os.Open\|ReadDir\|WalkDir" cmd/ internal/ | grep -v _test
grep -rn "keys.yml" cmd/ internal/ | grep -v _test

# 3. Download URL guards
grep -n "ReleaseBaseURL" internal/release/download.go
grep -n "DefaultAgentReleaseURLPrefix" internal/actions/update_agent.go

# 4. Agent and Dev Node signature verification paths
grep -n "verifyAgentBinarySignature" internal/actions/update_agent.go
grep -n "verifySignature" internal/actions/dev_node.go
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
│   ├── actions/          # action whitelist + handlers
│   ├── audit/            # append-only audit log
│   ├── config/           # config.yaml + state.yaml (platform-aware defaults)
│   ├── launchd/          # macOS plist renderer + launchctl wrapper
│   ├── logstream/        # node-log streaming (journalctl on Linux, tail -f on macOS)
│   ├── metrics/          # host + node-process sampling
│   ├── netinfo/          # public IP + country over HTTPS
│   ├── nodeinfo/         # parses `node --node-info`
│   ├── nodeinstall/      # install detection (binary / config / process / unit)
│   ├── reconcile/        # background loops: verify / du / version-poll
│   ├── release/          # node release downloader (const URL)
│   ├── rpcconfig/        # patches node config.yml RPC listen addrs
│   ├── svcctl/           # platform-agnostic service control interface
│   ├── systemd/          # Linux unit renderer + systemctl wrapper
│   ├── token/            # crypto/rand bearer-token gen + 0600 save
│   └── ws/               # outbound WebSocket client
├── install.sh            # Linux + macOS installer
├── migrate-macos-root.sh # migrate legacy macOS user-mode installs to system LaunchDaemons
├── uninstall.sh          # Linux + macOS uninstaller
├── remove-node.sh        # remove node only (keep agent)
└── Makefile
```

## License

MIT
