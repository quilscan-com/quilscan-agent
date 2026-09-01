#!/usr/bin/env bash
# quilscan-agent installer.
#
# Linux  : downloads the prebuilt binary to /usr/local/bin, registers a
#          systemd service, prints the pairing token. Requires sudo.
# macOS  : downloads to /usr/local/bin, registers a system LaunchDaemon,
#          prints the pairing token. Requires sudo so it stays alive across
#          user lock/logout.
#
# Environment (optional):
#   QSA_RELEASE_URL  Override the release asset prefix (for private mirrors)
#
# Usage:
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh | sudo bash    (macOS)
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh | sudo bash    (Linux)

set -euo pipefail

QSA_RELEASE_URL="${QSA_RELEASE_URL:-https://qstorage.quilibrium.com/quilscan-agent}"
AGENT_LABEL="com.quilscan.agent"

PLATFORM=""
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64) PLATFORM="linux-amd64" ;;
  Linux-aarch64) PLATFORM="linux-arm64" ;;
  Darwin-arm64) PLATFORM="darwin-arm64" ;;
  Darwin-x86_64)
    echo "Unsupported: Intel Macs are not provided by Quilibrium upstream." >&2
    exit 1 ;;
  *) echo "Unsupported platform: $(uname -s)-$(uname -m)"; exit 1 ;;
esac

print_node_blocker_message() {
  local os_label="$1"
  shift
  echo "" >&2
  echo "Existing Quilibrium node files were detected on this ${os_label}:" >&2
  for path in "$@"; do echo "  - $path" >&2; done
  echo "" >&2
  echo "quilscan-agent now requires a clean node environment before install." >&2
  echo "These files are treated as your own existing node and will not be adopted automatically." >&2
  echo "" >&2
  echo "Move or back up the listed files first, then re-run this installer." >&2
  echo "If you want Quilscan to manage that old node later, install the agent after cleanup and use Migrate Existing from the dashboard." >&2
}

pgrep_first() {
  pgrep -x "$1" 2>/dev/null | head -1 || true
}

install_staged_agent_binary() {
  local url="$1"
  local destination="$2"
  local destination_dir
  local temp_bin
  local version_output

  destination_dir="$(dirname "$destination")"
  mkdir -p "$destination_dir"
  temp_bin="$(mktemp "$destination_dir/.quilscan-agent.XXXXXX")"

  if ! curl -fsSL "$url" -o "$temp_bin"; then
    rm -f "$temp_bin"
    return 1
  fi
  if ! chmod 0755 "$temp_bin"; then
    rm -f "$temp_bin"
    return 1
  fi
  if ! version_output="$("$temp_bin" --version 2>/dev/null)"; then
    rm -f "$temp_bin"
    return 1
  fi
  if [[ -z "${version_output//[[:space:]]/}" ]]; then
    rm -f "$temp_bin"
    return 1
  fi
  if ! mv -f "$temp_bin" "$destination"; then
    rm -f "$temp_bin"
    return 1
  fi
}

# ─────────────────────────────────────────────────────────────
# macOS branch (LaunchDaemon / system-scope, requires sudo)
# ─────────────────────────────────────────────────────────────
install_macos() {
  if [[ $EUID -ne 0 ]]; then
    echo "Please run with sudo on macOS:" >&2
    echo "  curl -fsSL ${QSA_RELEASE_URL}/install.sh | sudo bash" >&2
    exit 1
  fi

  local bin_dir="/usr/local/bin"
  local app_support="/Library/Application Support/quilscan-agent"
  local logs_dir="/Library/Logs"
  local launch_dir="/Library/LaunchDaemons"
  local plist_path="$launch_dir/${AGENT_LABEL}.plist"
  local agent_bin="$bin_dir/quilscan-agent"
  local node_bin="$bin_dir/quilibrium-node"
  local token_path="$app_support/token"
  local target_user="${SUDO_USER:-}"
  local target_home=""
  local running_agent_pid=""
  if [[ -n "$target_user" && "$target_user" != "root" ]]; then
    target_home="$(dscl . -read "/Users/$target_user" NFSHomeDirectory 2>/dev/null | sed 's/^NFSHomeDirectory:[[:space:]]*//' || true)"
  fi
  running_agent_pid="$(pgrep_first quilscan-agent)"

  local legacy=()
  local legacy_launch_target=""
  local legacy_loaded=0
  if [[ -n "$target_home" ]]; then
    local legacy_agent_bin="$target_home/.local/bin/quilscan-agent"
    [[ -e "$legacy_agent_bin" ]] && legacy+=("$legacy_agent_bin")
    [[ -e "${legacy_agent_bin}.sig" ]] && legacy+=("${legacy_agent_bin}.sig")
    [[ -e "${legacy_agent_bin}.new" ]] && legacy+=("${legacy_agent_bin}.new")
    [[ -e "${legacy_agent_bin}.sig.new" ]] && legacy+=("${legacy_agent_bin}.sig.new")
    [[ -e "$target_home/Library/LaunchAgents/${AGENT_LABEL}.plist" ]] && legacy+=("$target_home/Library/LaunchAgents/${AGENT_LABEL}.plist")
    [[ -e "$target_home/Library/Application Support/quilscan-agent" ]] && legacy+=("$target_home/Library/Application Support/quilscan-agent")
    legacy_launch_target="gui/$(id -u "$target_user")/$AGENT_LABEL"
    if launchctl print "$legacy_launch_target" >/dev/null 2>&1; then
      legacy_loaded=1
    fi
  fi
  if [[ "$legacy_loaded" == "1" && ${#legacy[@]} -eq 0 ]]; then
    echo ""
    echo "[note] Stale legacy macOS quilscan-agent LaunchAgent registration detected:"
    echo "  - $legacy_launch_target"
    echo "[note] No legacy agent files were found under $target_home, so there is no token/config to migrate."
    echo "[note] Removing stale launchd registration and continuing with a fresh system install."
    launchctl bootout "$legacy_launch_target" >/dev/null 2>&1 || true
    running_agent_pid="$(pgrep_first quilscan-agent)"
  elif [[ "$legacy_loaded" == "1" ]]; then
    legacy+=("legacy LaunchAgent loaded for $target_user: $AGENT_LABEL")
  fi
  if [[ -n "$running_agent_pid" && ${#legacy[@]} -gt 0 ]]; then
    legacy+=("running legacy process: quilscan-agent (pid $running_agent_pid)")
  fi
  if (( ${#legacy[@]} > 0 )); then
    echo "" >&2
    echo "A legacy user-mode macOS quilscan-agent install was detected:" >&2
    for p in "${legacy[@]}"; do echo "  - $p" >&2; done
    echo "" >&2
    echo "Run uninstall.sh for the old user Agent, then rerun install.sh with sudo:" >&2
    echo "  curl -fsSL ${QSA_RELEASE_URL}/uninstall.sh | bash" >&2
    echo "  curl -fsSL ${QSA_RELEASE_URL}/install.sh | sudo bash" >&2
    exit 1
  fi

  # Pre-flight: refuse to overwrite an existing install. The uninstall path
  # (uninstall.sh) is the canonical way to back up + remove.
  local existing=()
  [[ -e "$agent_bin" ]] && existing+=("$agent_bin")
  [[ -e "$agent_bin.sig" ]] && existing+=("$agent_bin.sig")
  [[ -e "$agent_bin.new" ]] && existing+=("$agent_bin.new")
  [[ -e "$agent_bin.sig.new" ]] && existing+=("$agent_bin.sig.new")
  [[ -e "$plist_path" ]] && existing+=("$plist_path")
  [[ -e "$app_support" ]] && existing+=("$app_support")
  if launchctl print "system/$AGENT_LABEL" >/dev/null 2>&1; then
    existing+=("launchd job loaded: $AGENT_LABEL")
  fi
  if [[ -n "$running_agent_pid" ]]; then
    existing+=("running process: quilscan-agent (pid $running_agent_pid)")
  fi
  if (( ${#existing[@]} > 0 )); then
    echo "" >&2
    echo "An existing quilscan-agent install was detected:" >&2
    for p in "${existing[@]}"; do echo "  - $p" >&2; done
    echo "" >&2
    echo "Run the macOS uninstall first, then re-run this installer:" >&2
    echo "  curl -fsSL ${QSA_RELEASE_URL}/uninstall.sh | sudo bash" >&2
    exit 1
  fi

  local node_blockers=()
  [[ -e "$node_bin" ]] && node_blockers+=("$node_bin")
  [[ -e "$node_bin.dgst" ]] && node_blockers+=("$node_bin.dgst")
  for sig in "$node_bin".dgst.sig.*; do
    [[ -e "$sig" ]] && node_blockers+=("$sig")
  done
  [[ -e "$bin_dir/qclient" ]] && node_blockers+=("$bin_dir/qclient")
  [[ -e "$bin_dir/qclient.dgst" ]] && node_blockers+=("$bin_dir/qclient.dgst")
  [[ -e "$bin_dir/qclient.sig" ]] && node_blockers+=("$bin_dir/qclient.sig")
  for sig in "$bin_dir/qclient".dgst.sig.*; do
    [[ -e "$sig" ]] && node_blockers+=("$sig")
  done
  [[ -e "/Library/LaunchDaemons/com.quilscan.node.plist" ]] && node_blockers+=("/Library/LaunchDaemons/com.quilscan.node.plist")
  [[ -e "/Library/Application Support/quilibrium/.config" ]] && node_blockers+=("/Library/Application Support/quilibrium/.config")
  if [[ -n "$target_home" ]]; then
    [[ -e "$target_home/.local/bin/quilibrium-node" ]] && node_blockers+=("$target_home/.local/bin/quilibrium-node")
    [[ -e "$target_home/Library/LaunchAgents/com.quilscan.node.plist" ]] && node_blockers+=("$target_home/Library/LaunchAgents/com.quilscan.node.plist")
    [[ -e "$target_home/Library/Application Support/quilibrium/.config" ]] && node_blockers+=("$target_home/Library/Application Support/quilibrium/.config")
  fi
  local running_node_pid
  running_node_pid="$(pgrep_first quilibrium-node)"
  if [[ -n "$running_node_pid" ]]; then
    node_blockers+=("running process: quilibrium-node (pid $running_node_pid)")
  fi
  if (( ${#node_blockers[@]} > 0 )); then
    print_node_blocker_message "Mac" "${node_blockers[@]}"
    exit 1
  fi

  echo "[1/5] Downloading agent → $agent_bin"
  install_staged_agent_binary "$QSA_RELEASE_URL/quilscan-agent-$PLATFORM" "$agent_bin"

  echo "[2/5] Creating support directories"
  mkdir -p "$app_support" "$logs_dir" "$launch_dir"
  cat > "$app_support/config.yaml" <<YAML
backend_url: "wss://api.quilscan.com/api/agent/ws"
service_mode: "system"
service_user: "root"
node_service_mode: "system"
agent_binary_path: "/usr/local/bin/quilscan-agent"
node_binary_path: "/usr/local/bin/quilibrium-node"
qclient_binary_path: "/usr/local/bin/qclient"
qclient_release_url: "https://releases.quilscan.com"
download_proxy_url: ""
node_service_name: "com.quilscan.node"
agent_service_name: "com.quilscan.agent"
token_path: "/Library/Application Support/quilscan-agent/token"
state_path: "/Library/Application Support/quilscan-agent/state.yaml"
audit_log_path: "/Library/Logs/quilscan-agent.log"
unit_dir: "/Library/LaunchDaemons"
managed_config_dir: "/Library/Application Support/quilibrium/.config"
backup_root_dir: "/Library/Application Support/quilscan-agent/backups"
node_log_path: "/Library/Logs/quilibrium-node.log"
YAML

  echo "[3/5] Generating token"
  # init-token writes "$token_path" with 0600 inside the agent binary so the
  # token never lands in /tmp where any local user could read it.
  local token
  token="$("$agent_bin" init-token)"
  # Belt-and-braces: ensure perms in case the user's umask is unusual.
  chmod 600 "$token_path" 2>/dev/null || true

  echo "[4/5] Installing LaunchDaemon at $plist_path"
  cat > "$plist_path" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${AGENT_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${agent_bin}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ProcessType</key>
  <string>Background</string>
  <key>StandardOutPath</key>
  <string>/Library/Logs/quilscan-agent.log</string>
  <key>StandardErrorPath</key>
  <string>/Library/Logs/quilscan-agent.log</string>
</dict>
</plist>
PLIST

  chown root:wheel "$plist_path" "$app_support/config.yaml" "$token_path" 2>/dev/null || true
  chmod 644 "$plist_path" "$app_support/config.yaml"
  launchctl bootstrap system "$plist_path"
  launchctl kickstart -k "system/$AGENT_LABEL"
  # Brief pause + verify
  sleep 1
  if ! launchctl print "system/$AGENT_LABEL" 2>/dev/null | grep -q "state = running"; then
    echo "[warn] launchd loaded the job but it didn't reach 'running' state." >&2
    echo "       Check $logs_dir/quilscan-agent.log for details." >&2
  fi

  echo "[5/5] Done"
  echo
  echo "============================================================"
  echo "  YOUR TOKEN (copy this once, then paste into Quilscan):"
  echo
  echo "    $token"
  echo
  echo "  Store it securely — reinstall to regenerate."
  echo "============================================================"
  echo
  echo "Useful commands:"
  echo "  tail -f /Library/Logs/quilscan-agent.log"
  echo "      # live agent log"
  echo "  sudo launchctl print system/$AGENT_LABEL"
  echo "      # service state"
  echo "  sudo launchctl kickstart -k system/$AGENT_LABEL"
  echo "      # restart agent"
}

# ─────────────────────────────────────────────────────────────
# Linux branch (systemd / system-scope, requires sudo)
# ─────────────────────────────────────────────────────────────
install_linux() {
  if [[ $EUID -ne 0 ]]; then
    echo "Please run with sudo" >&2
    exit 1
  fi

  local existing=()
  [[ -e /usr/local/bin/quilscan-agent ]]                    && existing+=("/usr/local/bin/quilscan-agent")
  [[ -e /usr/local/bin/quilscan-agent.sig ]]                && existing+=("/usr/local/bin/quilscan-agent.sig")
  [[ -e /usr/local/bin/quilscan-agent.new ]]                && existing+=("/usr/local/bin/quilscan-agent.new")
  [[ -e /usr/local/bin/quilscan-agent.sig.new ]]            && existing+=("/usr/local/bin/quilscan-agent.sig.new")
  [[ -e /etc/quilscan-agent ]]                              && existing+=("/etc/quilscan-agent/")
  [[ -e /etc/systemd/system/quilscan-agent.service ]]       && existing+=("/etc/systemd/system/quilscan-agent.service")
  [[ -e /var/log/quilscan-agent.log ]]                      && existing+=("/var/log/quilscan-agent.log")
  local node_blockers=()
  [[ -e /var/lib/quilscan/node/.config ]]                   && node_blockers+=("/var/lib/quilscan/node/.config")
  [[ -e /usr/local/bin/quilibrium-node ]]                   && node_blockers+=("/usr/local/bin/quilibrium-node")
  [[ -e /usr/local/bin/quilibrium-node.dgst ]]              && node_blockers+=("/usr/local/bin/quilibrium-node.dgst")
  for sig in /usr/local/bin/quilibrium-node.dgst.sig.*; do
    [[ -e "$sig" ]] && node_blockers+=("$sig")
  done
  [[ -e /usr/local/bin/qclient ]]                           && node_blockers+=("/usr/local/bin/qclient")
  [[ -e /usr/local/bin/qclient.dgst ]]                      && node_blockers+=("/usr/local/bin/qclient.dgst")
  [[ -e /usr/local/bin/qclient.sig ]]                       && node_blockers+=("/usr/local/bin/qclient.sig")
  for sig in /usr/local/bin/qclient.dgst.sig.*; do
    [[ -e "$sig" ]] && node_blockers+=("$sig")
  done
  [[ -e /etc/systemd/system/quilibrium-node.service ]]      && node_blockers+=("/etc/systemd/system/quilibrium-node.service")

  if command -v systemctl >/dev/null 2>&1; then
    if systemctl list-unit-files --no-pager 2>/dev/null | grep -q '^quilscan-agent\.service'; then
      existing+=("systemd unit registered: quilscan-agent.service")
    fi
    if systemctl is-active --quiet quilscan-agent 2>/dev/null; then
      existing+=("systemd service running: quilscan-agent (active)")
    fi
    if systemctl is-active --quiet quilibrium-node 2>/dev/null; then
      node_blockers+=("systemd service running: quilibrium-node.service (active)")
    fi
  fi
  local running_agent_pid running_node_pid
  running_agent_pid="$(pgrep_first quilscan-agent)"
  running_node_pid="$(pgrep_first quilibrium-node)"
  if command -v pgrep >/dev/null 2>&1 && [[ -n "$running_agent_pid" ]]; then
    existing+=("running process: quilscan-agent (pid $running_agent_pid)")
  fi
  if command -v pgrep >/dev/null 2>&1 && [[ -n "$running_node_pid" ]]; then
    node_blockers+=("running process: quilibrium-node (pid $running_node_pid)")
  fi

  if (( ${#existing[@]} > 0 )); then
    echo "" >&2
    echo "An existing quilscan installation was detected at:" >&2
    for path in "${existing[@]}"; do echo "  - $path" >&2; done
    echo "" >&2
    echo "Run the agent uninstall script first, then re-run this installer:" >&2
    echo "The uninstall script does not remove Quilibrium node data." >&2
    echo "  curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash" >&2
    exit 1
  fi

  if (( ${#node_blockers[@]} > 0 )); then
    print_node_blocker_message "server" "${node_blockers[@]}"
    exit 1
  fi

  local bin_url="$QSA_RELEASE_URL/quilscan-agent-$PLATFORM"
  echo "[1/5] Downloading $bin_url"
  install_staged_agent_binary "$bin_url" /usr/local/bin/quilscan-agent

  echo "[2/5] Creating /etc/quilscan-agent/"
  mkdir -p /etc/quilscan-agent

  echo "[3/5] Generating token"
  local token
  token=$(/usr/local/bin/quilscan-agent init-token)

  echo "[4/5] Installing systemd service"
  curl -fsSL "$QSA_RELEASE_URL/quilscan-agent.service" -o /etc/systemd/system/quilscan-agent.service
  systemctl daemon-reload
  systemctl enable --now quilscan-agent

  echo "[5/5] Done"
  echo
  echo "============================================================"
  echo "  YOUR TOKEN (copy this once, then paste into Quilscan):"
  echo
  echo "    $token"
  echo
  echo "  Store it securely — reinstall to regenerate."
  echo "============================================================"
  echo
  echo "Useful commands:"
  echo "  journalctl -u quilscan-agent -f"
  echo "      # live agent log"
  echo "  systemctl status quilscan-agent"
  echo "      # service state"
  echo "  systemctl restart quilscan-agent"
  echo "      # restart agent"
}

case "$PLATFORM" in
  darwin-arm64) install_macos ;;
  *)            install_linux ;;
esac
