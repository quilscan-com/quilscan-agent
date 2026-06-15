#!/usr/bin/env bash
# Migrate an existing macOS quilscan-agent install from user LaunchAgents to
# root-owned system LaunchDaemons. This is intentionally a standalone script so
# operators can test the migration path before the default installer changes.
#
# Scope:
#   - If an existing user-level agent is found, migrate the agent.
#   - If that agent also has a managed node, migrate the node and qclient binaries/services.
#   - If only a node is found and no agent is found, do not migrate.
#   - Agent and node both run as root after migration.
#   - Existing node .config directories are not copied, moved, chowned, or rewritten.
#
# Usage:
#   curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/migrate-macos-root.sh | sudo bash -s -- --yes
#   sudo ./migrate-macos-root.sh --dry-run

set -euo pipefail

AGENT_LABEL="com.quilscan.agent"
NODE_LABEL="com.quilscan.node"
DEFAULT_BACKEND_URL="wss://api.quilscan.com/api/agent/ws"
QSA_RELEASE_URL="${QSA_RELEASE_URL:-https://qstorage.quilibrium.com/quilscan-agent}"
AGENT_PLATFORM="darwin-arm64"
NODE_FILE_LIMIT=524288
MAXFILES_LABEL="limit.maxfiles"
MAXFILES_PLIST="/Library/LaunchDaemons/${MAXFILES_LABEL}.plist"
SYSCTL_CONF="/etc/sysctl.conf"

TARGET_USER=""
YES=0
DRY_RUN="${DRY_RUN:-0}"
NO_START=0

usage() {
  cat <<USAGE
Usage:
  sudo $0 [--user <mac-user>] [--yes] [--dry-run] [--no-start]

Options:
  --user <mac-user>  User that owns the existing macOS user-mode install.
                     Defaults to SUDO_USER, then the console user.
  --yes              Run without an interactive confirmation prompt.
  --dry-run          Print planned actions without changing files/services.
  --no-start         Write files and plists but do not bootstrap services.
  -h, --help         Show this help.

Examples:
  curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/migrate-macos-root.sh | sudo bash -s -- --yes
  sudo $0 --dry-run
  sudo $0 --yes
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --user)
      [[ $# -ge 2 ]] || { echo "missing value for --user" >&2; exit 2; }
      TARGET_USER="$2"
      shift 2
      ;;
    --yes|-y)
      YES=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --no-start)
      NO_START=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

log() { printf '%s\n' "$*"; }
warn() { printf '[warn] %s\n' "$*" >&2; }
die() { printf '[error] %s\n' "$*" >&2; exit 1; }

quote_cmd() {
  local out="" arg
  for arg in "$@"; do
    case "$arg" in
      *[!A-Za-z0-9_./:=+-]*|"")
        arg="'${arg//\'/\'\\\'\'}'"
        ;;
    esac
    out="${out}${out:+ }${arg}"
  done
  printf '%s' "$out"
}

run() {
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ %s\n' "$(quote_cmd "$@")"
  else
    "$@"
  fi
}

write_file() {
  local path="$1"
  local mode="$2"
  local tmp
  tmp="$(mktemp "/tmp/quilscan-migrate.XXXXXX")"
  cat > "$tmp"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ write %s mode %s\n' "$path" "$mode"
    sed 's/^/  | /' "$tmp"
    rm -f "$tmp"
    return 0
  fi
  mkdir -p "$(dirname "$path")"
  install -m "$mode" "$tmp" "$path"
  rm -f "$tmp"
}

require_macos_root() {
  [[ "$(uname -s)" == "Darwin" ]] || die "this migration script only supports macOS"
  if [[ "${EUID}" -ne 0 && "$DRY_RUN" != "1" ]]; then
    die "please run with sudo"
  fi
  if [[ "${EUID}" -ne 0 && "$DRY_RUN" == "1" ]]; then
    warn "dry-run is running without sudo; service detection may be incomplete"
  fi
}

command_exists() { command -v "$1" >/dev/null 2>&1; }

require_commands() {
  local missing=() cmd
  for cmd in launchctl id dscl stat install ditto awk sed mktemp chown chmod curl sysctl cp; do
    command_exists "$cmd" || missing+=("$cmd")
  done
  if (( ${#missing[@]} > 0 )); then
    die "missing required command(s): ${missing[*]}"
  fi
}

detect_user() {
  if [[ -n "$TARGET_USER" ]]; then
    return
  fi
  if [[ -n "${SUDO_USER:-}" && "${SUDO_USER:-}" != "root" ]]; then
    TARGET_USER="$SUDO_USER"
    return
  fi
  TARGET_USER="$(stat -f %Su /dev/console 2>/dev/null || true)"
  if [[ -z "$TARGET_USER" || "$TARGET_USER" == "root" || "$TARGET_USER" == "loginwindow" ]]; then
    die "could not determine target user; pass --user <mac-user>"
  fi
}

user_home() {
  local user="$1"
  local home
  home="$(dscl . -read "/Users/$user" NFSHomeDirectory 2>/dev/null | sed 's/^NFSHomeDirectory:[[:space:]]*//' || true)"
  if [[ -n "$home" ]]; then
    printf '%s' "$home"
    return 0
  fi
  case "$user" in
    *[!A-Za-z0-9._-]*|"") return 1 ;;
  esac
  home="$(eval "printf '%s' ~$user" 2>/dev/null || true)"
  [[ -n "$home" && "$home" != "~$user" ]] || return 1
  printf '%s' "$home"
}

loaded() {
  local target="$1"
  launchctl print "$target" >/dev/null 2>&1
}

path_exists() {
  [[ -e "$1" || -L "$1" ]]
}

confirm_or_exit() {
  [[ "$YES" == "1" || "$DRY_RUN" == "1" ]] && return
  if [[ ! -r /dev/tty ]]; then
    die "confirmation requires a tty; rerun with --yes after reviewing --dry-run"
  fi
  printf 'Proceed with root/system migration? Type "migrate" to continue: ' > /dev/tty
  local ans
  read -r ans < /dev/tty
  [[ "$ans" == "migrate" ]] || die "aborted"
}

backup_root=""

backup_existing() {
  local path="$1"
  path_exists "$path" || return 0
  local rel="${path#/}"
  local dst="$backup_root/$rel"
  run mkdir -p "$(dirname "$dst")"
  run mv "$path" "$dst"
  log "backed up $path -> $dst"
}

copy_backup_existing() {
  local path="$1"
  path_exists "$path" || return 0
  local rel="${path#/}"
  local dst="$backup_root/$rel"
  run mkdir -p "$(dirname "$dst")"
  run cp -p "$path" "$dst"
  log "copied backup $path -> $dst"
}

first_existing_file() {
  local path
  for path in "$@"; do
    [[ -f "$path" ]] || continue
    printf '%s' "$path"
    return 0
  done
  return 1
}

same_file() {
  local a="$1"
  local b="$2"
  [[ -e "$a" && -e "$b" ]] || return 1
  local ai bi
  ai="$(stat -L -f '%d:%i' "$a" 2>/dev/null || true)"
  bi="$(stat -L -f '%d:%i' "$b" 2>/dev/null || true)"
  [[ -n "$ai" && "$ai" == "$bi" ]]
}

copy_file_with_mode() {
  local src="$1"
  local dst="$2"
  local mode="$3"
  [[ -f "$src" ]] || return 0
  if same_file "$src" "$dst"; then
    run chmod "$mode" "$dst"
    return 0
  fi
  backup_existing "$dst"
  run mkdir -p "$(dirname "$dst")"
  run install -m "$mode" "$src" "$dst"
}

copy_binary_bundle() {
  local src_prefix="$1"
  local dst_prefix="$2"
  [[ -f "$src_prefix" ]] || return 0
  copy_file_with_mode "$src_prefix" "$dst_prefix" 0755

  local src suffix dst
  for src in "$src_prefix".dgst "$src_prefix".sig "$src_prefix".dgst.sig.*; do
    [[ -e "$src" ]] || continue
    suffix="${src#$src_prefix}"
    dst="${dst_prefix}${suffix}"
    copy_file_with_mode "$src" "$dst" 0644
  done
}

latest_agent_tmp=""

cleanup_latest_agent_tmp() {
  [[ -n "$latest_agent_tmp" ]] || return 0
  rm -f "$latest_agent_tmp"
}
trap cleanup_latest_agent_tmp EXIT

prefetch_latest_agent() {
  local url="$QSA_RELEASE_URL/quilscan-agent-$AGENT_PLATFORM"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ curl -fsSL %s -o /tmp/quilscan-agent.%s.<tmp>\n' "$url" "$AGENT_PLATFORM"
    return 0
  fi
  log "Downloading latest agent from $url"
  latest_agent_tmp="$(mktemp "/tmp/quilscan-agent.${AGENT_PLATFORM}.XXXXXX")"
  curl -fsSL "$url" -o "$latest_agent_tmp"
  chmod +x "$latest_agent_tmp"
}

install_prefetched_agent() {
  local dst="$1"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ install prefetched latest agent to %s\n' "$dst"
    return 0
  fi
  [[ -n "$latest_agent_tmp" && -f "$latest_agent_tmp" ]] || die "latest agent was not downloaded"
  backup_existing "$dst"
  install -m 0755 "$latest_agent_tmp" "$dst"
}

copy_dir() {
  local src="$1"
  local dst="$2"
  [[ -d "$src" ]] || return 0
  backup_existing "$dst"
  run mkdir -p "$(dirname "$dst")"
  run ditto "$src" "$dst"
}

yaml_get_string() {
  local file="$1"
  local key="$2"
  [[ -f "$file" ]] || return 0
  awk -F: -v key="$key" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*:" {
      sub(/^[^:]*:[[:space:]]*/, "", $0)
      gsub(/^["'\''"]|["'\''"]$/, "", $0)
      print $0
      exit
    }
  ' "$file"
}

xml_escape() {
  local s="$1"
  s="${s//&/&amp;}"
  s="${s//</&lt;}"
  s="${s//>/&gt;}"
  s="${s//\"/&quot;}"
  s="${s//\'/&apos;}"
  printf '%s' "$s"
}

yaml_quote() {
  printf '"%s"' "$(printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g')"
}

yaml_set() {
  local file="$1"
  local key="$2"
  local value="$3"
  local quoted repl tmp
  quoted="$(yaml_quote "$value")"
  repl="${key}: ${quoted}"
  if [[ ! -f "$file" && "$DRY_RUN" == "1" ]]; then
    printf '+ update yaml %s key %s\n' "$file" "$key"
    return 0
  fi
  tmp="$(mktemp "/tmp/quilscan-state.XXXXXX")"
  awk -v key="$key" -v repl="$repl" '
    BEGIN { done = 0 }
    $0 ~ "^[[:space:]]*" key ":" {
      if (!done) {
        print repl
        done = 1
      }
      next
    }
    { print }
    END {
      if (!done) print repl
    }
  ' "$file" > "$tmp"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ update yaml %s key %s\n' "$file" "$key"
    rm -f "$tmp"
  else
    mv "$tmp" "$file"
  fi
}

yaml_set_raw() {
  local file="$1"
  local key="$2"
  local value="$3"
  local repl tmp
  repl="${key}: ${value}"
  if [[ ! -f "$file" && "$DRY_RUN" == "1" ]]; then
    printf '+ update yaml %s key %s\n' "$file" "$key"
    return 0
  fi
  tmp="$(mktemp "/tmp/quilscan-state.XXXXXX")"
  awk -v key="$key" -v repl="$repl" '
    BEGIN { done = 0 }
    $0 ~ "^[[:space:]]*" key ":" {
      if (!done) {
        print repl
        done = 1
      }
      next
    }
    { print }
    END {
      if (!done) print repl
    }
  ' "$file" > "$tmp"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ update yaml %s key %s\n' "$file" "$key"
    rm -f "$tmp"
  else
    mv "$tmp" "$file"
  fi
}

extract_backend_url() {
  local cfg="$1"
  [[ -f "$cfg" ]] || { printf '%s' "$DEFAULT_BACKEND_URL"; return; }
  local v
  v="$(awk -F: '
    /^[[:space:]]*backend_url[[:space:]]*:/ {
      sub(/^[^:]*:[[:space:]]*/, "", $0)
      gsub(/^["'\'']|["'\'']$/, "", $0)
      print $0
      exit
    }
  ' "$cfg")"
  if [[ -n "$v" ]]; then
    printf '%s' "$v"
  else
    printf '%s' "$DEFAULT_BACKEND_URL"
  fi
}

available_kb_for() {
  local path="$1"
  while [[ ! -e "$path" && "$path" != "/" ]]; do
    path="$(dirname "$path")"
  done
  df -k "$path" | awk 'NR == 2 { print $4 }'
}

require_space_for_copy() {
  local src="$1"
  local dst_parent="$2"
  [[ -d "$src" ]] || return 0
  local need avail
  need="$(du -sk "$src" | awk '{print $1}')"
  avail="$(available_kb_for "$dst_parent")"
  [[ -n "$need" && -n "$avail" ]] || return 0
  # Need + 10% headroom + 1 GiB for logs/sidecars.
  local required=$(( need + need / 10 + 1048576 ))
  if (( avail < required )); then
    die "not enough free space to copy $src: need about ${required}KB, available ${avail}KB"
  fi
}

write_agent_config() {
  local path="$1"
  local backend_url="$2"
  write_file "$path" 0644 <<YAML
backend_url: "${backend_url}"
service_mode: "system"
service_user: "root"
node_service_mode: "system"
agent_binary_path: "/usr/local/bin/quilscan-agent"
node_binary_path: "/usr/local/bin/quilibrium-node"
qclient_binary_path: "/usr/local/bin/qclient"
qclient_release_url: "https://releases.quilscan.com"
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
}

write_agent_plist() {
  local path="$1"
  write_file "$path" 0644 <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${AGENT_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/quilscan-agent</string>
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
}

write_node_plist() {
  local path="$1"
  local work_dir="$2"
  local work_dir_xml
  work_dir_xml="$(xml_escape "$work_dir")"
  write_file "$path" 0644 <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${NODE_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/quilibrium-node</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${work_dir_xml}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ProcessType</key>
  <string>Background</string>
  <key>SoftResourceLimits</key>
  <dict>
    <key>NumberOfFiles</key>
    <integer>${NODE_FILE_LIMIT}</integer>
  </dict>
  <key>HardResourceLimits</key>
  <dict>
    <key>NumberOfFiles</key>
    <integer>${NODE_FILE_LIMIT}</integer>
  </dict>
  <key>StandardOutPath</key>
  <string>/Library/Logs/quilibrium-node.log</string>
  <key>StandardErrorPath</key>
  <string>/Library/Logs/quilibrium-node.log</string>
</dict>
</plist>
PLIST
}

ensure_live_sysctl_at_least() {
  local key="$1"
  local target="$2"
  local current
  current="$(sysctl -n "$key" 2>/dev/null || printf '0')"
  case "$current" in
    ''|*[!0-9]*) current=0 ;;
  esac
  if (( current >= target )); then
    return 0
  fi
  run sysctl -w "${key}=${target}"
}

ensure_sysctl_conf_value() {
  local key="$1"
  local target="$2"
  local line="${key}=${target}"
  if [[ "$DRY_RUN" == "1" ]]; then
    printf '+ ensure %s in %s\n' "$line" "$SYSCTL_CONF"
    return 0
  fi
  touch "$SYSCTL_CONF"
  local tmp
  tmp="$(mktemp "/tmp/quilscan-sysctl.XXXXXX")"
  awk -v key="$key" -v line="$line" '
    BEGIN { done = 0 }
    {
      trimmed = $0
      sub(/^[ \t]+/, "", trimmed)
      if (trimmed ~ "^" key "([ \t=]|$)") {
        if (!done) {
          print line
          done = 1
        }
        next
      }
      print
    }
    END {
      if (!done) {
        print line
      }
    }
  ' "$SYSCTL_CONF" > "$tmp"
  install -m 0644 "$tmp" "$SYSCTL_CONF"
  rm -f "$tmp"
}

write_maxfiles_plist() {
  write_file "$MAXFILES_PLIST" 0644 <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${MAXFILES_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>launchctl</string>
    <string>limit</string>
    <string>maxfiles</string>
    <string>${NODE_FILE_LIMIT}</string>
    <string>${NODE_FILE_LIMIT}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>ServiceIPC</key>
  <false/>
</dict>
</plist>
PLIST
}

ensure_macos_file_limits() {
  log "ensuring macOS maxfiles limit ${NODE_FILE_LIMIT}"
  ensure_live_sysctl_at_least "kern.maxfiles" "$NODE_FILE_LIMIT"
  ensure_live_sysctl_at_least "kern.maxfilesperproc" "$NODE_FILE_LIMIT"
  copy_backup_existing "$SYSCTL_CONF"
  ensure_sysctl_conf_value "kern.maxfiles" "$NODE_FILE_LIMIT"
  ensure_sysctl_conf_value "kern.maxfilesperproc" "$NODE_FILE_LIMIT"
  copy_backup_existing "$MAXFILES_PLIST"
  write_maxfiles_plist
  run chown root:wheel "$MAXFILES_PLIST"
  run chmod 644 "$MAXFILES_PLIST"
  stop_service_if_loaded "system/$MAXFILES_LABEL"
  bootstrap_service "$MAXFILES_LABEL" "$MAXFILES_PLIST"
}

ensure_symlink() {
  local link="$1"
  local target="$2"
  if [[ -L "$link" && "$(readlink "$link")" == "$target" ]]; then
    return 0
  fi
  backup_existing "$link"
  run mkdir -p "$(dirname "$link")"
  run ln -s "$target" "$link"
}

stop_service_if_loaded() {
  local target="$1"
  if loaded "$target"; then
    run launchctl bootout "$target"
  fi
}

bootstrap_service() {
  local label="$1"
  local plist="$2"
  [[ "$NO_START" == "1" ]] && return 0
  run launchctl bootstrap system "$plist"
  run launchctl kickstart -k "system/$label"
}

verify_service() {
  local label="$1"
  [[ "$DRY_RUN" == "1" || "$NO_START" == "1" ]] && return 0
  if launchctl print "system/$label" >/dev/null 2>&1; then
    log "verified system/$label is loaded"
  else
    warn "system/$label is not loaded; check /Library/Logs and launchctl output"
  fi
}

main() {
  require_macos_root
  require_commands
  detect_user

  id "$TARGET_USER" >/dev/null 2>&1 || die "user not found: $TARGET_USER"
  local uid home
  uid="$(id -u "$TARGET_USER")"
  home="$(user_home "$TARGET_USER")"
  [[ -n "$home" && -d "$home" ]] || die "home directory not found for $TARGET_USER"

  local user_bin="$home/.local/bin"
  local user_agent_support="$home/Library/Application Support/quilscan-agent"
  local user_node_root="$home/Library/Application Support/quilibrium"
  local user_launch_agents="$home/Library/LaunchAgents"

  local src_agent_bin="$user_bin/quilscan-agent"
  local src_token="$user_agent_support/token"
  local src_state="$user_agent_support/state.yaml"
  local src_config="$user_agent_support/config.yaml"
  local src_agent_plist="$user_launch_agents/$AGENT_LABEL.plist"

  local src_node_bin="$user_bin/quilibrium-node"
  local src_qclient_bin="$user_bin/qclient"
  local src_node_config="$user_node_root/.config"
  local src_node_plist="$user_launch_agents/$NODE_LABEL.plist"

  local sys_agent_bin="/usr/local/bin/quilscan-agent"
  local sys_node_bin="/usr/local/bin/quilibrium-node"
  local sys_qclient_bin="/usr/local/bin/qclient"
  local sys_agent_support="/Library/Application Support/quilscan-agent"
  local sys_node_root="/Library/Application Support/quilibrium"
  local sys_node_config="$sys_node_root/.config"
  local sys_launch_daemons="/Library/LaunchDaemons"
  local sys_agent_plist="$sys_launch_daemons/$AGENT_LABEL.plist"
  local sys_node_plist="$sys_launch_daemons/$NODE_LABEL.plist"
  local sys_state="$sys_agent_support/state.yaml"
  local sys_config="$sys_agent_support/config.yaml"
  local recorded_node_config=""
  local node_work_dir=""
  local original_install_source=""
  local node_config_origin=""

  recorded_node_config="$(yaml_get_string "$src_state" "config_path")"
  original_install_source="$(yaml_get_string "$src_state" "install_source")"
  if [[ -z "$recorded_node_config" && -f "$sys_state" ]]; then
    recorded_node_config="$(yaml_get_string "$sys_state" "migrated_from")"
  fi
  if [[ -z "$recorded_node_config" && -f "$sys_state" ]]; then
    recorded_node_config="$(yaml_get_string "$sys_state" "config_path")"
  fi
  if [[ -z "$original_install_source" && -f "$sys_state" ]]; then
    original_install_source="$(yaml_get_string "$sys_state" "install_source")"
  fi
  if [[ -n "$recorded_node_config" ]]; then
    src_node_config="$recorded_node_config"
  fi
  node_work_dir="$(dirname "$src_node_config")"
  node_config_origin="$original_install_source"
  if [[ "$node_config_origin" != "fresh" && "$node_config_origin" != "migrated" ]]; then
    if [[ "$src_node_config" == "$user_node_root/.config" ]]; then
      node_config_origin="fresh"
    else
      node_config_origin="migrated"
    fi
  fi

  local has_agent=0
  local has_node=0
  local only_node=0

  if path_exists "$src_agent_bin" || path_exists "$src_token" || path_exists "$src_agent_plist" || loaded "gui/$uid/$AGENT_LABEL" || path_exists "$sys_agent_bin"; then
    has_agent=1
  fi
  if path_exists "$src_node_bin" || path_exists "$src_node_config" || path_exists "$src_node_plist" || loaded "gui/$uid/$NODE_LABEL" || path_exists "$sys_node_bin" || path_exists "$sys_node_config"; then
    has_node=1
  fi
  if [[ "$has_agent" == "0" && "$has_node" == "1" ]]; then
    only_node=1
  fi

  if [[ "$only_node" == "1" ]]; then
    log "A node was detected, but no quilscan-agent install was found for user $TARGET_USER."
    log "Per policy, node-only hosts are not migrated. Install or restore the agent first."
    exit 0
  fi
  if [[ "$has_agent" == "0" ]]; then
    log "No quilscan-agent install found for user $TARGET_USER; nothing to migrate."
    exit 0
  fi

  [[ -f "$src_token" || -f "$sys_agent_support/token" ]] || die "agent token not found at $src_token or $sys_agent_support/token"
  if [[ "$has_node" == "1" ]]; then
    [[ -f "$src_node_bin" || -f "$sys_node_bin" ]] || die "node was detected but binary is missing"
    [[ -d "$src_node_config" ]] || die "node was detected but .config directory is missing: $src_node_config"
  fi
  local token_src state_src config_src
  token_src="$(first_existing_file "$src_token" "$sys_agent_support/token" || true)"
  state_src="$(first_existing_file "$src_state" "$sys_state" || true)"
  config_src="$(first_existing_file "$src_config" "$sys_config" || true)"

  local ts
  ts="$(date -u +%Y%m%dT%H%M%SZ)"
  backup_root="$sys_agent_support/backups/macos-root-migration-$ts"

  log "macOS root migration plan:"
  log "  user:        $TARGET_USER ($uid)"
  log "  home:        $home"
  log "  migrate agent: yes"
  log "  migrate node:  $([[ "$has_node" == "1" ]] && printf yes || printf no)"
  if [[ "$has_node" == "1" ]]; then
    log "  node config:   $src_node_config"
    log "  config origin: $node_config_origin"
  fi
  log "  backup dir:    $backup_root"
  log "  dry run:       $DRY_RUN"
  log "  start:         $([[ "$NO_START" == "1" ]] && printf no || printf yes)"
  confirm_or_exit

  log "[0/7] Downloading latest agent"
  prefetch_latest_agent

  run mkdir -p "$sys_agent_support" "$sys_launch_daemons" "/Library/Logs" "$backup_root"

  log "[1/7] Stopping old and existing services"
  stop_service_if_loaded "gui/$uid/$AGENT_LABEL"
  stop_service_if_loaded "gui/$uid/$NODE_LABEL"
  stop_service_if_loaded "system/$AGENT_LABEL"
  stop_service_if_loaded "system/$NODE_LABEL"
  run pkill -x quilscan-agent || true
  if [[ "$has_node" == "1" ]]; then
    run pkill -x quilibrium-node || true
  fi

  log "[2/7] Installing root-owned agent files"
  install_prefetched_agent "$sys_agent_bin"
  if [[ ! -x "$sys_agent_bin" && "$DRY_RUN" != "1" ]]; then
    die "agent binary install failed"
  fi
  copy_file_with_mode "$token_src" "$sys_agent_support/token" 0600
  copy_file_with_mode "$state_src" "$sys_state" 0644
  if [[ ! -f "$sys_state" && "$DRY_RUN" != "1" ]]; then
    write_file "$sys_state" 0644 <<YAML
schema_version: 3
YAML
  fi
  local backend_url
  backend_url="$(extract_backend_url "$config_src")"
  backup_existing "$sys_config"
  write_agent_config "$sys_config" "$backend_url"

  log "[3/7] Installing root-owned node files"
  if [[ "$has_node" == "1" ]]; then
    copy_binary_bundle "$src_node_bin" "$sys_node_bin"
    copy_binary_bundle "$src_qclient_bin" "$sys_qclient_bin"
    log "preserving existing node .config at $src_node_config"
  else
    log "no managed node detected; skipping node migration"
  fi

  log "[4/7] Rewriting migrated state paths"
  if [[ -f "$sys_state" || "$DRY_RUN" == "1" ]]; then
    yaml_set_raw "$sys_state" "schema_version" "3"
    if [[ "$has_node" == "1" ]]; then
      yaml_set "$sys_state" "config_path" "$src_node_config"
      yaml_set "$sys_state" "binary_path" "$sys_node_bin"
      yaml_set "$sys_state" "service_unit" "$NODE_LABEL"
      if [[ -f "$sys_qclient_bin" || -f "$src_qclient_bin" || "$DRY_RUN" == "1" ]]; then
        yaml_set "$sys_state" "qclient_binary_path" "$sys_qclient_bin"
      fi
      yaml_set "$sys_state" "install_source" "$node_config_origin"
      if [[ "$node_config_origin" == "migrated" ]]; then
        yaml_set "$sys_state" "migrated_from" "$src_node_config"
      fi
    fi
  fi

  log "[5/7] Writing system LaunchDaemons"
  backup_existing "$sys_agent_plist"
  write_agent_plist "$sys_agent_plist"
  run chown root:wheel "$sys_agent_plist"
  run chmod 644 "$sys_agent_plist"
  if [[ "$has_node" == "1" ]]; then
    backup_existing "$sys_node_plist"
    write_node_plist "$sys_node_plist" "$node_work_dir"
    run chown root:wheel "$sys_node_plist"
    run chmod 644 "$sys_node_plist"
    ensure_macos_file_limits
  fi

  log "[6/7] Disabling old user LaunchAgents and backing up user-mode residues"
  backup_existing "$src_agent_plist"
  backup_existing "$src_agent_bin"
  backup_existing "$user_agent_support"
  if [[ "$has_node" == "1" ]]; then
    backup_existing "$src_node_plist"
    backup_existing "$src_node_bin"
    backup_existing "$src_qclient_bin"
  fi
  log "[6/7] Adding root compatibility links"
  ensure_symlink "/var/root/Library/Application Support/quilscan-agent" "$sys_agent_support"
  ensure_symlink "/var/root/.local/bin/quilscan-agent" "$sys_agent_bin"
  if [[ "$has_node" == "1" ]]; then
    ensure_symlink "/var/root/.local/bin/quilibrium-node" "$sys_node_bin"
    if [[ -f "$sys_qclient_bin" || -f "$src_qclient_bin" || "$DRY_RUN" == "1" ]]; then
      ensure_symlink "/var/root/.local/bin/qclient" "$sys_qclient_bin"
    fi
  fi

  log "[7/7] Starting system LaunchDaemons"
  bootstrap_service "$AGENT_LABEL" "$sys_agent_plist"
  if [[ "$has_node" == "1" ]]; then
    bootstrap_service "$NODE_LABEL" "$sys_node_plist"
  fi
  verify_service "$AGENT_LABEL"
  if [[ "$has_node" == "1" ]]; then
    verify_service "$NODE_LABEL"
  fi

  log ""
  log "Migration complete."
  log "Backups: $backup_root"
  log "Agent log: /Library/Logs/quilscan-agent.log"
  if [[ "$has_node" == "1" ]]; then
    log "Node log:  /Library/Logs/quilibrium-node.log"
  fi
  log ""
  log "Useful checks:"
  log "  sudo launchctl print system/$AGENT_LABEL"
  if [[ "$has_node" == "1" ]]; then
    log "  sudo launchctl print system/$NODE_LABEL"
  fi
}

main "$@"
