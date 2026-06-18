# Quilscan Agent

Quilscan Agent is the service that connects a Quilibrium node machine to
[Quilscan](https://quilscan.com). It pairs with Quilscan Node Console through a
locally generated token and lets the browser request approved node operations
without opening SSH.

## Supported Platforms

| Platform | Service manager | Install mode |
|---|---|---|
| Linux amd64 / arm64 | systemd | system service |
| macOS Apple Silicon | launchd | system LaunchDaemon |

Intel macOS is not supported.

## Before You Start

You need:

- A Linux server with systemd, or an Apple Silicon Mac.
- `sudo` access on that machine.
- Internet access from the machine to Quilscan and the release bucket.
- A browser that can open Quilscan Node Console.

Run the install command on the machine that will run the node.

## Install The Agent

Run:

```bash
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh | sudo bash
```

The installer will:

1. Detect your operating system and CPU architecture.
2. Download the correct `quilscan-agent` binary.
3. Create the agent support directory.
4. Generate a local pairing token.
5. Install and start the agent service.
6. Print the token once in the terminal.

Copy the token when it is printed.

If you need to read the token again:

```bash
# Linux
sudo cat /etc/quilscan-agent/token

# macOS
sudo cat "/Library/Application Support/quilscan-agent/token"
```

## Connect To Quilscan

Open:

```text
https://quilscan.com/node-console
```

Paste the token and click Connect.

After the agent is connected, Node Console shows the current machine state. For
a clean install, it should show that no node is installed yet.

## Install A Fresh Node

Use Fresh Install when this machine does not already have a Quilibrium node that
you want to keep.

In Node Console:

1. Open the connected agent.
2. Choose Fresh install.
3. Select the available node version or source.
4. Start the install.
5. Wait for the result dialog.
6. Start the node from Node Console after install finishes.

For fresh installs, Quilscan manages:

- The node binary.
- The node service.
- The fresh node `.config` directory.
- The qclient binary.
- Start, stop, update, and qclient actions.

Fresh install paths:

| Item | Linux | macOS |
|---|---|---|
| Node binary | `/usr/local/bin/quilibrium-node` | `/usr/local/bin/quilibrium-node` |
| Node service | `/etc/systemd/system/quilibrium-node.service` | `/Library/LaunchDaemons/com.quilscan.node.plist` |
| Node `.config` | `/var/lib/quilscan/node/.config` | `/Library/Application Support/quilibrium/.config` |
| qclient binary | `/usr/local/bin/qclient` | `/usr/local/bin/qclient` |

## Operate The Node

After a node is installed, use Node Console for routine operations:

- Start node.
- Stop node.
- Restart node.
- Update agent.
- Update node.
- Switch node source when available.
- Install or update qclient.
- Refresh allocations.
- Run qclient manage actions.
- Check public TCP and UDP port reachability.
- Clean detected residue.
- Remove the managed node.
- Delete node store backups.

Command results open in a dialog. Failed commands also open in a dialog so long
error output does not break the page layout.

## Check Logs

Linux:

```bash
sudo journalctl -u quilscan-agent -f
sudo systemctl status quilscan-agent

sudo journalctl -u quilibrium-node -f
sudo systemctl status quilibrium-node
```

macOS:

```bash
tail -f /Library/Logs/quilscan-agent.log
sudo launchctl print system/com.quilscan.agent

tail -f /Library/Logs/quilibrium-node.log
sudo launchctl print system/com.quilscan.node
```

## Restart Services Manually

Linux:

```bash
sudo systemctl restart quilscan-agent
sudo systemctl restart quilibrium-node
```

macOS:

```bash
sudo launchctl kickstart -k system/com.quilscan.agent
sudo launchctl kickstart -k system/com.quilscan.node
```

## Update The Agent

The recommended update path is the Update button in Node Console.

Agent updates verify the downloaded binary signature before replacement. If the
signature is missing or invalid, the update fails and the old agent binary stays
in place.

## Remove Only The Managed Node

Use this when you want to keep the agent connected to Quilscan but remove the
managed node from the machine:

```bash
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/remove-node.sh | sudo bash
```

The script stops the managed node service and moves managed node artifacts into
a backup directory when possible. The agent remains installed and paired.

Backup locations:

| Platform | Backup root |
|---|---|
| Linux | `/var/lib/quilscan/backups/` |
| macOS | `/Library/Application Support/quilscan-agent/backups/` |

After removal, reconnect in Node Console and install a fresh node when needed.

## Uninstall The Agent

Use this when you no longer want this machine connected to Quilscan:

```bash
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash
```

The uninstall script removes:

- The agent binary.
- The agent service definition.
- The agent config, token, state, and log.

The uninstall script preserves existing node files if they are present.

## Common Troubleshooting

### The installer says an existing agent was detected

Uninstall the existing agent first:

```bash
curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash
```

Then run the installer again.

### The installer says existing node files were detected

The installer does not overwrite existing node files during a fresh agent
install. Move or back up the listed files, then run the installer again.

### The agent is connected but no node is installed

This is normal after installing only the agent. Use Fresh install in Node
Console.

### The node command failed

Open the result dialog in Node Console, then check the local node log:

```bash
# Linux
sudo journalctl -u quilibrium-node -n 200 --no-pager

# macOS
tail -n 200 /Library/Logs/quilibrium-node.log
```

### The agent does not reconnect

Check that the service is running:

```bash
# Linux
sudo systemctl status quilscan-agent

# macOS
sudo launchctl print system/com.quilscan.agent
```

Then check the agent log:

```bash
# Linux
sudo journalctl -u quilscan-agent -n 200 --no-pager

# macOS
tail -n 200 /Library/Logs/quilscan-agent.log
```

## What The Agent Can Do

The backend can only request handlers registered in the agent binary. Common
user-facing actions include:

- `install`
- `start`
- `stop`
- `rescan`
- `refresh_qclient_allocations`
- `check_public_stream_ports`
- `qclient_manage_action`
- `restart_agent`
- `update_agent`
- `update_node`
- `switch_node_source`
- `cleanup_residue`
- `delete_node_store`
- `delete_node_store_backup`
- `install_qclient`

The complete whitelist is wired in `cmd/agent/main.go`. Everything else is
rejected by `internal/actions/dispatcher.go`.

## Safety Model

- Outbound only: the agent opens a WebSocket to Quilscan and does not listen on
  a public port.
- Local token: the pairing token is generated locally and stored with restricted
  permissions.
- Action whitelist: only hardcoded actions can run.
- Key files stay local: node key files such as `keys.yml` are not uploaded.
- Downloads are constrained: node downloads use the configured release base URL;
  agent self-updates require the trusted release prefix and an Ed25519
  signature check before replacement.
- Optional download proxy: `download_proxy_url` in `config.yaml` only affects
  release, dev-node, qclient, and agent-update downloads. It does not proxy the
  Quilscan WebSocket connection.
- Human-readable audit log: every received action is appended to the local agent
  log.

## File Layout

| Item | Linux | macOS |
|---|---|---|
| Agent binary | `/usr/local/bin/quilscan-agent` | `/usr/local/bin/quilscan-agent` |
| Agent service | `/etc/systemd/system/quilscan-agent.service` | `/Library/LaunchDaemons/com.quilscan.agent.plist` |
| Agent config | `/etc/quilscan-agent/config.yaml` | `/Library/Application Support/quilscan-agent/config.yaml` |
| Token | `/etc/quilscan-agent/token` | `/Library/Application Support/quilscan-agent/token` |
| State | `/etc/quilscan-agent/state.yaml` | `/Library/Application Support/quilscan-agent/state.yaml` |
| Agent log | `/var/log/quilscan-agent.log` | `/Library/Logs/quilscan-agent.log` |
| Node binary | `/usr/local/bin/quilibrium-node` | `/usr/local/bin/quilibrium-node` |
| Fresh node `.config` | `/var/lib/quilscan/node/.config` | `/Library/Application Support/quilibrium/.config` |
| Node log | `journalctl -u quilibrium-node` | `/Library/Logs/quilibrium-node.log` |

`state.yaml` is created after node or qclient state is first persisted. A fresh
agent-only install can have `config.yaml` and `token` without `state.yaml`.

### Download Proxy

If release downloads are slow from a Mac or server, add a local proxy to the
agent config and restart the agent:

```yaml
download_proxy_url: "http://127.0.0.1:7897"
```

`socks5://127.0.0.1:7897` is also supported. If the scheme is omitted, the
agent treats the value as an HTTP proxy. On macOS system-mode installs, the
agent runs as root, so the proxy must be reachable from the root LaunchDaemon
process on `127.0.0.1`.

## Install, Remove, And Uninstall Scripts

| Script | Purpose |
|---|---|
| `install.sh` | Install the agent as a system service and print the pairing token. |
| `remove-node.sh` | Remove the managed node and qclient while keeping the agent paired. |
| `uninstall.sh` | Remove the agent only. Existing node files are preserved. |

## Agent Self-Update Verification

Agent self-updates are verified before the running binary is replaced.

For each platform release, publish the binary and detached signature:

```text
quilscan-agent-linux-amd64
quilscan-agent-linux-amd64.sig
quilscan-agent-linux-arm64
quilscan-agent-linux-arm64.sig
quilscan-agent-darwin-arm64
quilscan-agent-darwin-arm64.sig
```

The update action only accepts URLs under:

```text
https://qstorage.quilibrium.com/quilscan-agent
```

During an update, the agent downloads the selected platform binary, downloads
the `.sig` file from the same URL, verifies the Ed25519 signature with the
public key built into the agent, then replaces the installed binary and restarts
the service. If verification fails, the current binary remains in place.

## Dev Node Public Builds

Quilscan Dev Node binaries are separate from official Quilibrium release
artifacts. They are built in a public repository so users can inspect the
upstream source commit, workflow definition, build logs, published SHA-256, and
detached signature for each platform.

When installing, switching to, or updating a Dev Node, the agent reads
`node-version.json`, downloads the platform artifact, verifies the detached
signature with the public key pinned into the agent binary, checks the SHA-256,
then replaces only the managed node binary. Config files, keys, peer ID, worker
store, and node data are not replaced.

Unsigned Dev Node artifacts are refused.

## Verify The Security Claims

```bash
git clone https://github.com/quilscan-com/quilscan-agent.git
cd quilscan-agent

# Registered action handlers.
sed -n '/Handlers: map\[string\]actions.Handler{/,/var collector/p' cmd/agent/main.go \
  | grep -n '^[[:space:]]*"[a-z_]*":'
grep -n "ErrForbidden" internal/actions/dispatcher.go

# Local file reads. Key files such as keys.yml should not be opened directly.
grep -rn "ReadFile\|ioutil.ReadFile\|os.Open\|ReadDir\|WalkDir" cmd/ internal/ | grep -v _test
grep -rn "keys.yml" cmd/ internal/ | grep -v _test

# Download guards.
grep -n "ReleaseBaseURL" internal/release/download.go
grep -n "DefaultAgentReleaseURLPrefix" internal/actions/update_agent.go

# Signature verification paths.
grep -n "verifyAgentBinarySignature" internal/actions/update_agent.go
grep -n "verifySignature" internal/actions/dev_node.go
```

If an audit result does not match the expected behavior, do not trust the
binary.

## Build From Source

```bash
git clone https://github.com/quilscan-com/quilscan-agent.git
cd quilscan-agent
make build
```

The build writes platform binaries to `dist/`.

Requires Go 1.22 or newer.

## GitHub Signed Builds

The `Build signed agent artifacts` GitHub Actions workflow builds and signs the
three release artifacts:

```text
quilscan-agent-linux-amd64
quilscan-agent-linux-arm64
quilscan-agent-darwin-arm64
```

It runs only when manually started with `workflow_dispatch`. The workflow
requires the repository secret `AGENT_SIGNING_PRIVATE_KEY`, containing the
base64-encoded Ed25519 private key. It uploads the binaries, `.sig` files, and
`SHA256SUMS` as workflow artifacts.

## Directory Structure

```text
quilscan-agent/
├── cmd/agent/main.go       # runtime entrypoint and action wiring
├── internal/
│   ├── actions/            # action handlers
│   ├── audit/              # append-only action log
│   ├── config/             # config.yaml and state.yaml defaults
│   ├── launchd/            # macOS LaunchDaemon helpers
│   ├── logstream/          # node-log streaming
│   ├── metrics/            # host and node-process metrics
│   ├── netinfo/            # public IP and country lookup
│   ├── nodeinfo/           # node-info parsing
│   ├── nodeinstall/        # install detection
│   ├── reconcile/          # background verification and version polling
│   ├── release/            # release manifest/download helpers
│   ├── rpcconfig/          # local RPC config patching
│   ├── svcctl/             # service-control abstraction
│   ├── systemd/            # Linux systemd helpers
│   ├── token/              # local token generation
│   └── ws/                 # outbound WebSocket client
├── install.sh
├── remove-node.sh
├── uninstall.sh
└── Makefile
```

## License

MIT
