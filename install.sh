#!/usr/bin/env bash
# quilscan-agent installer.
#
# Linux  : downloads the prebuilt binary to /usr/local/bin, registers a
#          systemd service, prints the pairing token. Requires sudo.
# macOS  : downloads to ~/.local/bin (creates dir + adds to PATH if missing),
#          registers a user-level LaunchAgent, prints the pairing token.
#          Does NOT require sudo — everything stays under the current user.
#
# Environment (optional):
#   QSA_RELEASE_URL  Override the release asset prefix (for private mirrors)
#
# Usage:
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh | bash              (macOS)
#   sudo curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/install.sh | sudo bash    (Linux)

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

# ─────────────────────────────────────────────────────────────
# macOS branch (LaunchAgent / user-scope, no sudo)
# ─────────────────────────────────────────────────────────────
install_macos() {
  local home="$HOME"
  local bin_dir="$home/.local/bin"
  local app_support="$home/Library/Application Support/quilscan-agent"
  local logs_dir="$home/Library/Logs"
  local launch_dir="$home/Library/LaunchAgents"
  local plist_path="$launch_dir/${AGENT_LABEL}.plist"
  local agent_bin="$bin_dir/quilscan-agent"
  local node_bin="$bin_dir/quilibrium-node"
  local token_path="$app_support/token"

  # Pre-flight: refuse to overwrite an existing install. The uninstall path
  # (uninstall.sh) is the canonical way to back up + remove.
  local existing=()
  [[ -e "$agent_bin" ]] && existing+=("$agent_bin")
  [[ -e "$plist_path" ]] && existing+=("$plist_path")
  [[ -e "$app_support" ]] && existing+=("$app_support")
  if launchctl print "gui/$(id -u)/$AGENT_LABEL" >/dev/null 2>&1; then
    existing+=("launchd job loaded: $AGENT_LABEL")
  fi
  if pgrep -x quilscan-agent >/dev/null 2>&1; then
    existing+=("running process: quilscan-agent (pid $(pgrep -x quilscan-agent | head -1))")
  fi
  if (( ${#existing[@]} > 0 )); then
    echo "" >&2
    echo "An existing quilscan-agent install was detected:" >&2
    for p in "${existing[@]}"; do echo "  - $p" >&2; done
    echo "" >&2
    echo "Run the macOS uninstall first, then re-run this installer:" >&2
    echo "  curl -fsSL ${QSA_RELEASE_URL}/uninstall.sh | bash" >&2
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
  [[ -e "$home/Library/LaunchAgents/com.quilscan.node.plist" ]] && node_blockers+=("$home/Library/LaunchAgents/com.quilscan.node.plist")
  [[ -e "$home/Library/Application Support/quilibrium/.config" ]] && node_blockers+=("$home/Library/Application Support/quilibrium/.config")
  if pgrep -x quilibrium-node >/dev/null 2>&1; then
    node_blockers+=("running process: quilibrium-node (pid $(pgrep -x quilibrium-node | head -1))")
  fi
  if (( ${#node_blockers[@]} > 0 )); then
    print_node_blocker_message "Mac" "${node_blockers[@]}"
    exit 1
  fi

  # Make sure ~/.local/bin exists and is on PATH for future shells. We modify
  # the user's shell rc only when the directory is missing from PATH; this
  # avoids appending duplicate lines on re-installs.
  mkdir -p "$bin_dir"
  if ! echo ":$PATH:" | grep -q ":$bin_dir:"; then
    local shell_name rc
    shell_name=$(basename "${SHELL:-zsh}")
    case "$shell_name" in
      zsh)  rc="$home/.zshrc" ;;
      bash) rc="$home/.bash_profile" ;;
      *)    rc="$home/.profile" ;;
    esac
    echo "" >> "$rc"
    echo '# Added by quilscan-agent installer' >> "$rc"
    echo 'export PATH="$HOME/.local/bin:$PATH"' >> "$rc"
    echo "[note] Added $bin_dir to PATH in $rc — open a new terminal or run: source \"$rc\""
  fi

  echo "[1/5] Downloading agent → $agent_bin"
  curl -fsSL "$QSA_RELEASE_URL/quilscan-agent-$PLATFORM" -o "$agent_bin"
  chmod +x "$agent_bin"

  echo "[2/5] Creating support directories"
  mkdir -p "$app_support" "$logs_dir" "$launch_dir"

  echo "[3/5] Generating token"
  # init-token writes "$token_path" with 0600 inside the agent binary so the
  # token never lands in /tmp where any local user could read it.
  local token
  token="$("$agent_bin" init-token)"
  # Belt-and-braces: ensure perms in case the user's umask is unusual.
  chmod 600 "$token_path" 2>/dev/null || true

  echo "[4/5] Installing LaunchAgent at $plist_path"
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
  <string>${logs_dir}/quilscan-agent.log</string>
  <key>StandardErrorPath</key>
  <string>${logs_dir}/quilscan-agent.log</string>
</dict>
</plist>
PLIST

  launchctl bootstrap "gui/$(id -u)" "$plist_path"
  # Brief pause + verify
  sleep 1
  if ! launchctl print "gui/$(id -u)/$AGENT_LABEL" 2>/dev/null | grep -q "state = running"; then
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
  echo "  tail -f $logs_dir/quilscan-agent.log"
  echo "      # live agent log"
  echo "  launchctl print gui/$(id -u)/$AGENT_LABEL"
  echo "      # service state"
  echo "  launchctl kickstart -k gui/$(id -u)/$AGENT_LABEL"
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
  if command -v pgrep >/dev/null 2>&1 && pgrep -x quilscan-agent >/dev/null 2>&1; then
    existing+=("running process: quilscan-agent (pid $(pgrep -x quilscan-agent | head -1))")
  fi
  if command -v pgrep >/dev/null 2>&1 && pgrep -x quilibrium-node >/dev/null 2>&1; then
    node_blockers+=("running process: quilibrium-node (pid $(pgrep -x quilibrium-node | head -1))")
  fi

  if (( ${#existing[@]} > 0 )); then
    echo "" >&2
    echo "An existing quilscan installation was detected at:" >&2
    for path in "${existing[@]}"; do echo "  - $path" >&2; done
    echo "" >&2
    echo "Run the agent uninstall script first, then re-run this installer:" >&2
    echo "The uninstall script does not remove Quilibrium node data." >&2
    echo "  sudo curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash" >&2
    exit 1
  fi

  if (( ${#node_blockers[@]} > 0 )); then
    print_node_blocker_message "server" "${node_blockers[@]}"
    exit 1
  fi

  local bin_url="$QSA_RELEASE_URL/quilscan-agent-$PLATFORM"
  echo "[1/5] Downloading $bin_url"
  curl -fsSL "$bin_url" -o /usr/local/bin/quilscan-agent
  chmod +x /usr/local/bin/quilscan-agent

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
