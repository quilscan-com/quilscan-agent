#!/usr/bin/env bash
# remove-node.sh — back up and stop the Quilibrium node managed by
# quilscan-agent. Also backs up the managed qclient bundle. Reads the agent
# state file to decide whether the install was fresh or migrated;
# user-imported .config dirs are preserved at their original path. The agent
# itself is paused while node artifacts are removed, then restored so the user
# can re-install or migrate again from the browser console without re-pairing.
#
# Linux : reads /etc/quilscan-agent/state.yaml, uses systemd to stop the
#         node, backs up under /var/lib/quilscan/backups/. Requires sudo.
# macOS : supports both legacy user LaunchAgents and root LaunchDaemons.
#         Fresh/default .config dirs are backed up; imported .config dirs are
#         preserved at their original path.
#
# Usage:
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/remove-node.sh | sudo bash    (macOS system-mode)
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/remove-node.sh | bash         (macOS legacy user-mode)
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/remove-node.sh | sudo bash    (Linux)

set -euo pipefail

AGENT_RESTORE_ACTIVE=0
AGENT_RESTORE_DONE=0
AGENT_RESTORE_KIND=""
AGENT_BOOTSTRAP_DOMAIN=""
AGENT_LAUNCH_TARGET=""
AGENT_PLIST=""

restore_agent_service() {
  if [[ "$AGENT_RESTORE_DONE" == "1" ]]; then
    return 0
  fi
  AGENT_RESTORE_DONE=1
  if [[ "$AGENT_RESTORE_ACTIVE" != "1" ]]; then
    return 0
  fi
  if [[ "$AGENT_RESTORE_KIND" == "linux" ]]; then
    if systemctl start quilscan-agent.service; then
      echo "[remove-node] Agent service restored."
      return 0
    else
      echo "[remove-node] ERROR: failed to restore quilscan-agent.service." >&2
      return 1
    fi
  fi
  if [[ "$AGENT_RESTORE_KIND" == "macos" ]]; then
    if [[ ! -f "$AGENT_PLIST" ]]; then
      echo "[remove-node] ERROR: Agent plist missing during restore: $AGENT_PLIST" >&2
      return 1
    fi
    if launchctl bootstrap "$AGENT_BOOTSTRAP_DOMAIN" "$AGENT_PLIST" && launchctl kickstart -k "$AGENT_LAUNCH_TARGET"; then
      echo "[remove-node] Agent service restored."
      return 0
    else
      echo "[remove-node] ERROR: failed to restore Agent service: $AGENT_LAUNCH_TARGET" >&2
      return 1
    fi
  fi
  echo "[remove-node] ERROR: Agent restore intent was invalid." >&2
  return 1
}

cleanup_remove_node() {
  local status=$?
  local restore_status=0
  trap - EXIT HUP INT TERM
  restore_agent_service || restore_status=$?
  if [[ "$status" -eq 0 && "$restore_status" -ne 0 ]]; then
    exit "$restore_status"
  fi
  exit "$status"
}

trap cleanup_remove_node EXIT
trap 'exit 1' HUP INT TERM

pause_agent_linux() {
  if systemctl is-active --quiet quilscan-agent.service; then
    if [[ ! -f /etc/systemd/system/quilscan-agent.service ]]; then
      echo "[remove-node] ERROR: Agent systemd unit missing; Node files were not changed." >&2
      return 1
    fi
    AGENT_RESTORE_KIND="linux"
    AGENT_RESTORE_ACTIVE=1
    if ! systemctl stop quilscan-agent.service; then
      echo "[remove-node] ERROR: failed to pause quilscan-agent.service; Node files were not changed." >&2
      return 1
    fi
  fi
}

pause_agent_macos() {
  AGENT_BOOTSTRAP_DOMAIN="$1"
  AGENT_LAUNCH_TARGET="$2"
  AGENT_PLIST="$3"
  if launchctl print "$AGENT_LAUNCH_TARGET" >/dev/null 2>&1; then
    if [[ ! -f "$AGENT_PLIST" ]]; then
      echo "[remove-node] ERROR: Agent plist missing; Node files were not changed: $AGENT_PLIST" >&2
      return 1
    fi
    AGENT_RESTORE_KIND="macos"
    AGENT_RESTORE_ACTIVE=1
    if ! launchctl bootout "$AGENT_LAUNCH_TARGET"; then
      echo "[remove-node] ERROR: failed to pause Agent service; Node files were not changed." >&2
      return 1
    fi
  fi
}

verify_agent_ownership_linux() {
  local missing=()
  [[ -f /usr/local/bin/quilscan-agent ]] || missing+=("/usr/local/bin/quilscan-agent")
  [[ -f /etc/systemd/system/quilscan-agent.service ]] || missing+=("/etc/systemd/system/quilscan-agent.service")
  [[ -f /etc/quilscan-agent/state.yaml ]] || missing+=("/etc/quilscan-agent/state.yaml")
  if (( ${#missing[@]} > 0 )); then
    echo "[remove-node] Agent is uninstalled or incomplete; Node files are user-managed and nothing changed." >&2
    for path in "${missing[@]}"; do echo "  - missing: $path" >&2; done
    return 1
  fi
}

verify_agent_ownership_macos() {
  local agent_bin="$1"
  local agent_plist="$2"
  local state="$3"
  local missing=()
  [[ -f "$agent_bin" ]] || missing+=("$agent_bin")
  [[ -f "$agent_plist" ]] || missing+=("$agent_plist")
  [[ -f "$state" ]] || missing+=("$state")
  if (( ${#missing[@]} > 0 )); then
    echo "[remove-node] Agent is uninstalled or incomplete; Node files are user-managed and nothing changed." >&2
    for path in "${missing[@]}"; do echo "  - missing: $path" >&2; done
    return 1
  fi
}

remove_binary_bundle() {
  local binary="$1"
  local sidecar
  rm -f "$binary" "$binary.dgst" "$binary.sig"
  for sidecar in "$binary".dgst.sig.*; do
    [[ -e "$sidecar" ]] && rm -f "$sidecar"
  done
}

backup_default_config() {
  local config_dir="$1"
  local backup_dir="$2"
  local destination
  local suffix=2
  [[ -e "$config_dir" ]] || return 0
  mkdir -p "$backup_dir" || return 1
  destination="$backup_dir/$(basename "$config_dir")"
  while [[ -e "$destination" ]]; do
    destination="$backup_dir/$(basename "$config_dir")-$suffix"
    suffix=$((suffix + 1))
  done
  if mv "$config_dir" "$destination"; then
    BACKED_UP_CONFIG="$config_dir -> $destination"
    return 0
  fi
  if ! cp -a "$config_dir" "$destination"; then
    echo "[remove-node] ERROR: failed to back up default config: $config_dir" >&2
    return 1
  fi
  if ! rm -rf "$config_dir"; then
    echo "[remove-node] ERROR: copied default config but failed to remove original: $config_dir" >&2
    return 1
  fi
  BACKED_UP_CONFIG="$config_dir -> $destination"
}

remove_node_macos() {
  local home="$HOME"
  local state="$home/Library/Application Support/quilscan-agent/state.yaml"
  local bin="$home/.local/bin/quilibrium-node"
  local qclient_bin="$home/.local/bin/qclient"
  local plist="$home/Library/LaunchAgents/com.quilscan.node.plist"
  local fresh_cfg="$home/Library/Application Support/quilibrium/.config"
  local label="com.quilscan.node"
  local launch_target="gui/$(id -u)/$label"
  local service_scope="user"
  local agent_plist="$home/Library/LaunchAgents/com.quilscan.agent.plist"
  local agent_bin="$home/.local/bin/quilscan-agent"
  local agent_target="gui/$(id -u)/com.quilscan.agent"
  local agent_domain="gui/$(id -u)"
  if [ "${EUID:-$(id -u)}" -eq 0 ]; then
    state="/Library/Application Support/quilscan-agent/state.yaml"
    bin="/usr/local/bin/quilibrium-node"
    qclient_bin="/usr/local/bin/qclient"
    plist="/Library/LaunchDaemons/com.quilscan.node.plist"
    fresh_cfg="/Library/Application Support/quilibrium/.config"
    launch_target="system/$label"
    service_scope="system"
    agent_plist="/Library/LaunchDaemons/com.quilscan.agent.plist"
    agent_bin="/usr/local/bin/quilscan-agent"
    agent_target="system/com.quilscan.agent"
    agent_domain="system"
  elif [ -f "/Library/Application Support/quilscan-agent/state.yaml" ] || launchctl print "system/$label" >/dev/null 2>&1; then
    echo "System-mode node detected. Re-run with sudo:" >&2
    echo "  curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/remove-node.sh | sudo bash" >&2
    exit 1
  fi
  verify_agent_ownership_macos "$agent_bin" "$agent_plist" "$state"
  local ts
  ts=$(date -u +%Y%m%d-%H%M%S)
  local backup
  if [ "$service_scope" = "system" ]; then
    backup="/Library/Application Support/quilscan-agent/backups/node-removal-$ts"
  else
    backup="$home/Library/Application Support/quilscan-agent/backups/node-removal-$ts"
  fi

  local install_source="unknown"
  local user_cfg=""
  local config_path=""
  if [ -f "$state" ]; then
    install_source=$(awk -F': *' '/^install_source:/ {print $2; exit}' "$state" | tr -d '"')
    user_cfg=$(awk -F': *' '/^migrated_from:/ {print $2; exit}' "$state" | tr -d '"')
    config_path=$(awk -F': *' '/^config_path:/ {print $2; exit}' "$state" | tr -d '"')
  fi
  echo "[remove-node] service_scope=$service_scope install_source=${install_source:-unknown}"
  pause_agent_macos "$agent_domain" "$agent_target" "$agent_plist"

  # Stop the launchd job if loaded so the node exits cleanly.
  if launchctl print "$launch_target" >/dev/null 2>&1; then
    launchctl bootout "$launch_target" 2>/dev/null || true
  fi
  # Belt-and-braces: terminate any orphan node process launchd did not catch
  # (e.g. if the user manually launched it once).
  pkill -TERM -x quilibrium-node 2>/dev/null || true
  sleep 2
  pkill -KILL -x quilibrium-node 2>/dev/null || true

  local removed=()
  BACKED_UP_CONFIG=""
  remove_binary_bundle "$bin"
  remove_binary_bundle "$qclient_bin"
  removed+=("$bin bundle" "$qclient_bin bundle")
  if [ "$service_scope" = "system" ]; then
    remove_binary_bundle "/var/root/.local/bin/quilibrium-node"
    remove_binary_bundle "/var/root/.local/bin/qclient"
  fi
  rm -f "$plist" "$state"
  removed+=("$plist" "$state")
  if [ "$install_source" = "fresh" ]; then
    backup_default_config "$fresh_cfg" "$backup"
  fi
  # A node that is still winding down can recreate .config after the first
  # move. Do one final stop-and-sweep pass so fresh installs are actually
  # clean after this script exits.
  pkill -TERM -x quilibrium-node 2>/dev/null || true
  sleep 1
  pkill -KILL -x quilibrium-node 2>/dev/null || true
  remove_binary_bundle "$bin"
  remove_binary_bundle "$qclient_bin"
  rm -f "$plist" "$state"
  if [ "$install_source" = "fresh" ]; then
    backup_default_config "$fresh_cfg" "$backup"
  fi
  # macOS has no daemon-reload equivalent — bootout already detached.

  echo
  echo "[remove-node] Removal complete."
  echo "[remove-node] Removed artifacts:"
  for item in "${removed[@]}"; do echo "  - $item"; done
  if [ -n "$BACKED_UP_CONFIG" ]; then
    echo "[remove-node] Backed up default config: $BACKED_UP_CONFIG"
  fi
  if [ "$install_source" = "migrated" ] && [ -n "$user_cfg" ]; then
    echo "[remove-node] Preserved (untouched): $user_cfg"
  elif [ "$install_source" = "migrated" ] && [ -n "$config_path" ]; then
    echo "[remove-node] Preserved (untouched): $config_path"
  fi
}

remove_node_linux() {
  local STATE=/etc/quilscan-agent/state.yaml
  local BIN=/usr/local/bin/quilibrium-node
  local QCLIENT_BIN=/usr/local/bin/qclient
  local UNIT_FILE=/etc/systemd/system/quilibrium-node.service
  local FRESH_CFG=/var/lib/quilscan/node/.config
  local TS
  TS=$(date -u +%Y%m%d-%H%M%S)
  local BACKUP=/var/lib/quilscan/backups/node-removal-$TS

  local INSTALL_SOURCE="unknown"
  local USER_CFG=""
  local CONFIG_PATH=""
  verify_agent_ownership_linux
  if [ -f "$STATE" ]; then
    INSTALL_SOURCE=$(awk -F': *' '/^install_source:/ {print $2; exit}' "$STATE" | tr -d '"')
    USER_CFG=$(awk -F': *' '/^migrated_from:/ {print $2; exit}' "$STATE" | tr -d '"')
    CONFIG_PATH=$(awk -F': *' '/^config_path:/ {print $2; exit}' "$STATE" | tr -d '"')
  fi
  echo "[remove-node] install_source=${INSTALL_SOURCE:-unknown}"
  pause_agent_linux

  systemctl stop quilibrium-node.service 2>/dev/null || true
  systemctl disable quilibrium-node.service 2>/dev/null || true
  pkill -TERM -x quilibrium-node 2>/dev/null || true
  sleep 2
  pkill -KILL -x quilibrium-node 2>/dev/null || true

  local REMOVED=()
  BACKED_UP_CONFIG=""
  remove_binary_bundle "$BIN"
  remove_binary_bundle "$QCLIENT_BIN"
  rm -f "$UNIT_FILE" "$STATE"
  REMOVED+=("$BIN bundle" "$QCLIENT_BIN bundle" "$UNIT_FILE" "$STATE")
  if [ "$INSTALL_SOURCE" = "fresh" ]; then
    backup_default_config "$FRESH_CFG" "$BACKUP"
  fi
  # A node that is still winding down can recreate .config after the first
  # move. Do one final stop-and-sweep pass so fresh installs are actually
  # clean after this script exits.
  pkill -TERM -x quilibrium-node 2>/dev/null || true
  sleep 1
  pkill -KILL -x quilibrium-node 2>/dev/null || true
  remove_binary_bundle "$BIN"
  remove_binary_bundle "$QCLIENT_BIN"
  rm -f "$UNIT_FILE" "$STATE"
  if [ "$INSTALL_SOURCE" = "fresh" ]; then
    backup_default_config "$FRESH_CFG" "$BACKUP"
  fi
  systemctl daemon-reload || true
  rmdir /etc/quilscan-agent 2>/dev/null || true

  echo
  echo "[remove-node] Removal complete."
  echo "[remove-node] Removed artifacts:"
  for item in "${REMOVED[@]}"; do echo "  - $item"; done
  if [ -n "$BACKED_UP_CONFIG" ]; then
    echo "[remove-node] Backed up default config: $BACKED_UP_CONFIG"
  fi
  if [ "$INSTALL_SOURCE" = "migrated" ] && [ -n "$USER_CFG" ]; then
    echo "[remove-node] Preserved (untouched): $USER_CFG"
  elif [ "$INSTALL_SOURCE" = "migrated" ] && [ -n "$CONFIG_PATH" ]; then
    echo "[remove-node] Preserved (untouched): $CONFIG_PATH"
  fi
}

case "$(uname -s)" in
  Darwin) remove_node_macos ;;
  *)      remove_node_linux ;;
esac
