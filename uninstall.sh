#!/usr/bin/env bash
# Remove quilscan-agent cleanly. Branches by platform: Linux uses systemd
# system paths and requires sudo; macOS uses user-level LaunchAgents and
# does not require sudo.
#
# Preserved intentionally on both platforms:
#   - The Quilibrium node binary
#   - The Quilibrium node service definition (systemd unit / launchd plist)
#   - The fresh-install node .config directory
#   - Any imported .config directory
#   - Node keys, worker-store, and data files
#
# Usage:
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | bash              (macOS)
#   sudo curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash    (Linux)

set -euo pipefail

AGENT_LABEL="com.quilscan.agent"

uninstall_macos() {
  local home="$HOME"
  local bin="$home/.local/bin/quilscan-agent"
  local app_support="$home/Library/Application Support/quilscan-agent"
  local plist="$home/Library/LaunchAgents/${AGENT_LABEL}.plist"
  local log="$home/Library/Logs/quilscan-agent.log"

  echo "[1/3] Stopping quilscan-agent (LaunchAgent)"
  if launchctl print "gui/$(id -u)/$AGENT_LABEL" >/dev/null 2>&1; then
    launchctl bootout "gui/$(id -u)/$AGENT_LABEL" || true
  fi
  pkill -x quilscan-agent 2>/dev/null || true

  echo "[2/3] Removing agent binary, plist, support directory"
  rm -f "$bin" "$plist" "$log"
  rm -rf "$app_support"

  echo "[3/3] Done"
  echo
  # Only list "preserved" items that actually exist on disk — printing
  # paths the user never created is misleading.
  local preserved=()
  [[ -e "$home/.local/bin/quilibrium-node" ]] && preserved+=("$home/.local/bin/quilibrium-node")
  [[ -e "$home/Library/LaunchAgents/com.quilscan.node.plist" ]] && preserved+=("$home/Library/LaunchAgents/com.quilscan.node.plist")
  [[ -e "$home/Library/Application Support/quilibrium/.config" ]] && preserved+=("$home/Library/Application Support/quilibrium/.config")
  if (( ${#preserved[@]} > 0 )); then
    echo "Removed quilscan-agent only. Preserved Quilibrium node files:"
    for p in "${preserved[@]}"; do echo "  - $p"; done
    echo "  - any imported .config directory at its original path"
  else
    echo "Removed quilscan-agent. No Quilibrium node files were present on this machine."
  fi
  echo
  echo "(The PATH line in your shell rc is left in place so a future reinstall works."
  echo " Remove it manually if you no longer want $home/.local/bin in PATH.)"
}

uninstall_linux() {
  if [[ $EUID -ne 0 ]]; then
    echo "Please run with sudo" >&2
    exit 1
  fi

  echo "[1/4] Stopping quilscan-agent"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl stop quilscan-agent.service 2>/dev/null || true
    systemctl disable quilscan-agent.service 2>/dev/null || true
  fi
  if command -v pkill >/dev/null 2>&1; then
    pkill -x quilscan-agent 2>/dev/null || true
  fi

  echo "[2/4] Removing agent binary and systemd unit"
  rm -f /usr/local/bin/quilscan-agent
  rm -f /etc/systemd/system/quilscan-agent.service

  echo "[3/4] Removing agent token, state, config, and log"
  rm -rf /etc/quilscan-agent
  rm -f /var/log/quilscan-agent.log

  echo "[4/4] Reloading systemd"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    systemctl reset-failed quilscan-agent.service 2>/dev/null || true
  fi

  echo
  # Only list "preserved" items that actually exist on disk.
  local preserved=()
  [[ -e /usr/local/bin/quilibrium-node ]] && preserved+=("/usr/local/bin/quilibrium-node")
  [[ -e /etc/systemd/system/quilibrium-node.service ]] && preserved+=("/etc/systemd/system/quilibrium-node.service")
  [[ -e /var/lib/quilscan/node/.config ]] && preserved+=("/var/lib/quilscan/node/.config")
  if (( ${#preserved[@]} > 0 )); then
    echo "Done. Removed quilscan-agent only. Preserved Quilibrium node files:"
    for p in "${preserved[@]}"; do echo "  - $p"; done
    echo "  - any imported .config directory at its original path"
  else
    echo "Done. Removed quilscan-agent. No Quilibrium node files were present on this server."
  fi
}

case "$(uname -s)" in
  Darwin) uninstall_macos ;;
  *)      uninstall_linux ;;
esac
