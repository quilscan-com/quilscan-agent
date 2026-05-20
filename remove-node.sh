#!/usr/bin/env bash
# remove-node.sh — back up and stop the Quilibrium node managed by
# quilscan-agent. Also backs up the managed qclient bundle. Reads the agent
# state file to decide whether the install was fresh or migrated;
# user-imported .config dirs are preserved at their original path. The agent
# itself is left running so the user can re-install or migrate again from the
# browser console without re-pairing.
#
# Linux : reads /etc/quilscan-agent/state.yaml, uses systemd to stop the
#         node, backs up under /var/lib/quilscan/backups/. Requires sudo.
# macOS : reads ~/Library/Application Support/quilscan-agent/state.yaml,
#         uses launchctl to stop the node, backs up under
#         ~/Library/Application Support/quilscan-agent/backups/. No sudo.
#
# Usage:
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/remove-node.sh | bash         (macOS)
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/remove-node.sh | sudo bash    (Linux)

set -euo pipefail

remove_node_macos() {
  local home="$HOME"
  local state="$home/Library/Application Support/quilscan-agent/state.yaml"
  local bin="$home/.local/bin/quilibrium-node"
  local qclient_bin="$home/.local/bin/qclient"
  local plist="$home/Library/LaunchAgents/com.quilscan.node.plist"
  local fresh_cfg="$home/Library/Application Support/quilibrium/.config"
  local label="com.quilscan.node"
  local ts
  ts=$(date -u +%Y%m%d-%H%M%S)
  local backup="$home/Library/Application Support/quilscan-agent/backups/node-removal-$ts"

  local install_source="unknown"
  local user_cfg=""
  if [ -f "$state" ]; then
    install_source=$(awk -F': *' '/^install_source:/ {print $2; exit}' "$state" | tr -d '"')
    user_cfg=$(awk -F': *' '/^migrated_from:/ {print $2; exit}' "$state" | tr -d '"')
    qclient_bin=$(awk -F': *' '/^qclient_binary_path:/ {print $2; exit}' "$state" | tr -d '"')
    qclient_bin=${qclient_bin:-"$home/.local/bin/qclient"}
  fi
  echo "[remove-node] install_source=${install_source:-unknown}"
  mkdir -p "$backup"

  # Stop the LaunchAgent if loaded so the node exits cleanly.
  if launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1; then
    launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  fi
  # Belt-and-braces: terminate any orphan node process the LaunchAgent
  # didn't catch (e.g. if the user manually launched it once).
  pkill -TERM -x quilibrium-node 2>/dev/null || true
  sleep 2
  pkill -KILL -x quilibrium-node 2>/dev/null || true

  local items=()
  move_to_backup() {
    local src="$1"
    [ -e "$src" ] || return 0
    local dst="$backup/$(basename "$src")"
    if mv "$src" "$dst" 2>/dev/null; then
      items+=("$src -> $dst")
      return 0
    fi
    if cp -a "$src" "$dst"; then
      rm -rf "$src"
      items+=("$src -> $dst")
    fi
  }
  move_binary_bundle_to_backup() {
    local binary="$1"
    local sig
    move_to_backup "$binary"
    move_to_backup "$binary.dgst"
    for sig in "$binary".dgst.sig.*; do
      [ -e "$sig" ] || continue
      move_to_backup "$sig"
    done
  }

  move_binary_bundle_to_backup "$bin"
  move_binary_bundle_to_backup "$qclient_bin"
  move_to_backup "$plist"
  move_to_backup "$state"
  if [ "$install_source" = "fresh" ]; then
    move_to_backup "$fresh_cfg"
  fi
  # macOS has no daemon-reload equivalent — bootout already detached.

  echo
  echo "[remove-node] Removal complete."
  echo "[remove-node] Backup directory: $backup"
  if [ ${#items[@]} -gt 0 ]; then
    echo "[remove-node] Items moved:"
    for it in "${items[@]}"; do echo "  - $it"; done
  else
    echo "[remove-node] No artifacts needed to be moved."
  fi
  if [ "$install_source" = "migrated" ] && [ -n "$user_cfg" ]; then
    echo "[remove-node] Preserved (untouched): $user_cfg"
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
  if [ -f "$STATE" ]; then
    INSTALL_SOURCE=$(awk -F': *' '/^install_source:/ {print $2; exit}' "$STATE" | tr -d '"')
    USER_CFG=$(awk -F': *' '/^migrated_from:/ {print $2; exit}' "$STATE" | tr -d '"')
    QCLIENT_BIN=$(awk -F': *' '/^qclient_binary_path:/ {print $2; exit}' "$STATE" | tr -d '"')
    QCLIENT_BIN=${QCLIENT_BIN:-/usr/local/bin/qclient}
  fi
  echo "[remove-node] install_source=${INSTALL_SOURCE:-unknown}"
  mkdir -p "$BACKUP"

  systemctl stop quilibrium-node.service 2>/dev/null || true
  systemctl disable quilibrium-node.service 2>/dev/null || true
  pkill -TERM -x quilibrium-node 2>/dev/null || true
  sleep 2
  pkill -KILL -x quilibrium-node 2>/dev/null || true

  local ITEMS=()
  move_to_backup() {
    local src="$1"
    [ -e "$src" ] || return 0
    local dst="$BACKUP/$(basename "$src")"
    if mv "$src" "$dst" 2>/dev/null; then
      ITEMS+=("$src -> $dst")
      return 0
    fi
    if cp -a "$src" "$dst"; then
      rm -rf "$src"
      ITEMS+=("$src -> $dst")
    fi
  }
  move_binary_bundle_to_backup() {
    local binary="$1"
    local sig
    move_to_backup "$binary"
    move_to_backup "$binary.dgst"
    for sig in "$binary".dgst.sig.*; do
      [ -e "$sig" ] || continue
      move_to_backup "$sig"
    done
  }
  move_binary_bundle_to_backup "$BIN"
  move_binary_bundle_to_backup "$QCLIENT_BIN"
  move_to_backup "$UNIT_FILE"
  move_to_backup "$STATE"
  if [ "$INSTALL_SOURCE" = "fresh" ]; then
    move_to_backup "$FRESH_CFG"
  fi
  systemctl daemon-reload || true

  echo
  echo "[remove-node] Removal complete."
  echo "[remove-node] Backup directory: $BACKUP"
  if [ ${#ITEMS[@]} -gt 0 ]; then
    echo "[remove-node] Items moved:"
    for it in "${ITEMS[@]}"; do echo "  - $it"; done
  else
    echo "[remove-node] No artifacts needed to be moved."
  fi
  if [ "$INSTALL_SOURCE" = "migrated" ] && [ -n "$USER_CFG" ]; then
    echo "[remove-node] Preserved (untouched): $USER_CFG"
  fi
}

case "$(uname -s)" in
  Darwin) remove_node_macos ;;
  *)      remove_node_linux ;;
esac
