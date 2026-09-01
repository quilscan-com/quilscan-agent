#!/usr/bin/env bash
# Remove quilscan-agent cleanly. Branches by platform: Linux uses systemd
# system paths and requires sudo; macOS supports both system LaunchDaemons
# and legacy user LaunchAgents.
#
# Preserved intentionally on both platforms:
#   - The Quilibrium node binary
#   - The Quilibrium node service definition (systemd unit / launchd plist)
#   - The fresh-install node .config directory
#   - Any imported .config directory
#   - Node keys, worker-store, and data files
#
# Usage:
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash         (macOS system-mode)
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | bash              (macOS legacy user-mode)
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash         (Linux)

set -euo pipefail

AGENT_LABEL="com.quilscan.agent"

remove_agent_binary_files() {
  local bin="$1"
  rm -f "$bin" "$bin.sig" "$bin.new" "$bin.sig.new"
}

remove_agent_support_files() {
  local support="$1"
  [[ -d "$support" ]] || return 0
  rm -f \
    "$support/token" "$support/token.new" "$support/token.tmp" \
    "$support/state.yaml" "$support/state.yaml.new" "$support/state.yaml.tmp" \
    "$support/config.yaml" "$support/config.yaml.new" "$support/config.yaml.tmp" \
    "$support/config.yml" "$support/config.yml.new" "$support/config.yml.tmp"
  rmdir "$support" 2>/dev/null || true
}

append_preserved_if_exists() {
  [[ -e "$1" ]] && preserved+=("$1")
}

append_backup_if_exists() {
  [[ -e "$1" ]] && backups+=("$1")
}

uninstall_macos() {
  local home="$HOME"
  local bin="$home/.local/bin/quilscan-agent"
  local app_support="$home/Library/Application Support/quilscan-agent"
  local plist="$home/Library/LaunchAgents/${AGENT_LABEL}.plist"
  local log="$home/Library/Logs/quilscan-agent.log"
  local launch_target="gui/$(id -u)/$AGENT_LABEL"
  local mode="legacy user"
  local target_user="${SUDO_USER:-}"
  local target_home=""
  local target_uid=""
  if [[ ${EUID:-$(id -u)} -eq 0 ]]; then
    bin="/usr/local/bin/quilscan-agent"
    app_support="/Library/Application Support/quilscan-agent"
    plist="/Library/LaunchDaemons/${AGENT_LABEL}.plist"
    log="/Library/Logs/quilscan-agent.log"
    launch_target="system/$AGENT_LABEL"
    mode="system"
    if [[ -n "$target_user" && "$target_user" != "root" ]]; then
      target_uid="$(id -u "$target_user" 2>/dev/null || true)"
      target_home="$(dscl . -read "/Users/$target_user" NFSHomeDirectory 2>/dev/null | sed 's/^NFSHomeDirectory:[[:space:]]*//' || true)"
    fi
  elif [[ -e "/Library/LaunchDaemons/${AGENT_LABEL}.plist" || -e "/Library/Application Support/quilscan-agent" || -e "/usr/local/bin/quilscan-agent" || -e "/usr/local/bin/quilscan-agent.sig" || -e "/usr/local/bin/quilscan-agent.new" || -e "/usr/local/bin/quilscan-agent.sig.new" ]]; then
    echo "System-mode macOS agent detected. Re-run with sudo:" >&2
    echo "  curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash" >&2
    exit 1
  fi

  echo "[1/3] Stopping quilscan-agent ($mode)"
  if launchctl print "$launch_target" >/dev/null 2>&1; then
    launchctl bootout "$launch_target" || true
  fi
  if [[ "$mode" = "system" && -n "$target_uid" ]] && launchctl print "gui/$target_uid/$AGENT_LABEL" >/dev/null 2>&1; then
    launchctl bootout "gui/$target_uid/$AGENT_LABEL" || true
  fi
  pkill -x quilscan-agent 2>/dev/null || true

  echo "[2/3] Removing agent binary, plist, and known support files"
  remove_agent_binary_files "$bin"
  rm -f "$plist" "$log"
  remove_agent_support_files "$app_support"
  if [[ "$mode" = "system" ]]; then
    remove_agent_binary_files "/var/root/.local/bin/quilscan-agent"
    rm -f "/var/root/Library/LaunchAgents/${AGENT_LABEL}.plist"
    rm -f "/var/root/Library/Logs/quilscan-agent.log"
    remove_agent_support_files "/var/root/Library/Application Support/quilscan-agent"
    if [[ -n "$target_home" && -d "$target_home" ]]; then
      remove_agent_binary_files "$target_home/.local/bin/quilscan-agent"
      rm -f "$target_home/Library/LaunchAgents/${AGENT_LABEL}.plist"
      rm -f "$target_home/Library/Logs/quilscan-agent.log"
      remove_agent_support_files "$target_home/Library/Application Support/quilscan-agent"
    fi
  fi

  echo "[3/3] Done"
  echo
  # Only list "preserved" items that actually exist on disk — printing
  # paths the user never created is misleading.
  local preserved=()
  local backups=()
  if [[ "$mode" = "system" ]]; then
    append_preserved_if_exists /usr/local/bin/quilibrium-node
    append_preserved_if_exists /usr/local/bin/qclient
    append_preserved_if_exists /Library/LaunchDaemons/com.quilscan.node.plist
    append_preserved_if_exists "/Library/Application Support/quilibrium/.config"
    append_backup_if_exists "/Library/Application Support/quilscan-agent/backups"
    append_backup_if_exists "/var/root/Library/Application Support/quilscan-agent/backups"
    if [[ -n "$target_home" ]]; then
      append_preserved_if_exists "$target_home/.local/bin/quilibrium-node"
      append_preserved_if_exists "$target_home/.local/bin/qclient"
      append_preserved_if_exists "$target_home/Library/LaunchAgents/com.quilscan.node.plist"
      append_preserved_if_exists "$target_home/Library/Application Support/quilibrium/.config"
      append_backup_if_exists "$target_home/Library/Application Support/quilscan-agent/backups"
    fi
  else
    append_preserved_if_exists "$home/.local/bin/quilibrium-node"
    append_preserved_if_exists "$home/.local/bin/qclient"
    append_preserved_if_exists "$home/Library/LaunchAgents/com.quilscan.node.plist"
    append_preserved_if_exists "$home/Library/Application Support/quilibrium/.config"
    append_backup_if_exists "$home/Library/Application Support/quilscan-agent/backups"
  fi
  if (( ${#preserved[@]} > 0 )); then
    echo "Removed quilscan-agent only. Preserved Quilibrium node and qclient files:"
    for p in "${preserved[@]}"; do echo "  - $p"; done
  else
    echo "Removed quilscan-agent. No default Quilibrium node or qclient runtime files were present on this machine."
  fi
  echo "  - any imported .config directory at its original path"
  if (( ${#backups[@]} > 0 )); then
    echo "Preserved Agent backup directories:"
    for p in "${backups[@]}"; do echo "  - $p"; done
  fi
  echo "Reinstall requires no Node/qclient runtime and no default managed .config."
  echo "Run remove-node.sh first if those remain; imported configs outside the default path stay untouched."
  echo
  if [[ "$mode" != "system" ]]; then
    echo "(The PATH line in your shell rc is left in place so a future reinstall works."
    echo " Remove it manually if you no longer want $home/.local/bin in PATH.)"
  fi
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
  remove_agent_binary_files /usr/local/bin/quilscan-agent
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
  local backups=()
  append_preserved_if_exists /usr/local/bin/quilibrium-node
  append_preserved_if_exists /usr/local/bin/qclient
  append_preserved_if_exists /etc/systemd/system/quilibrium-node.service
  append_preserved_if_exists /var/lib/quilscan/node/.config
  append_backup_if_exists /var/lib/quilscan/backups
  if (( ${#preserved[@]} > 0 )); then
    echo "Done. Removed quilscan-agent only. Preserved Quilibrium node and qclient files:"
    for p in "${preserved[@]}"; do echo "  - $p"; done
  else
    echo "Done. Removed quilscan-agent. No default Quilibrium node or qclient runtime files were present on this server."
  fi
  echo "  - any imported .config directory at its original path"
  if (( ${#backups[@]} > 0 )); then
    echo "Preserved Agent backup directories:"
    for p in "${backups[@]}"; do echo "  - $p"; done
  fi
  echo "Reinstall requires no Node/qclient runtime and no default managed .config."
  echo "Run remove-node.sh first if those remain; imported configs outside the default path stay untouched."
}

case "$(uname -s)" in
  Darwin) uninstall_macos ;;
  *)      uninstall_linux ;;
esac
