#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
INSTALL="$ROOT/install.sh"
UNINSTALL="$ROOT/uninstall.sh"

if grep -q '\[\[ -e /var/lib/quilscan \]\].*EXISTING' "$INSTALL"; then
  echo "install.sh must not treat preserved /var/lib/quilscan node data as an agent install conflict" >&2
  exit 1
fi

if grep -q 'mv /var/lib/quilscan ' "$INSTALL"; then
  echo "install.sh cleanup hint must not tell users to move preserved node data" >&2
  exit 1
fi

grep -q 'sudo curl -fsSL https://qstorage.quilibrium.com/quilscan-agent/uninstall.sh | sudo bash' "$INSTALL" || {
  echo "install.sh must direct users with agent leftovers to run the uninstall script" >&2
  exit 1
}

grep -q 'does not remove Quilibrium node data' "$INSTALL" || {
  echo "install.sh must say the uninstall script does not remove node data" >&2
  exit 1
}

grep -q '/var/lib/quilscan/node/.config' "$UNINSTALL" || {
  echo "uninstall.sh must explicitly preserve the managed node .config directory" >&2
  exit 1
}

grep -q '/var/lib/quilscan/node/.config' "$INSTALL" || {
  echo "install.sh must tell users that preserved node config/data do not block agent reinstall" >&2
  exit 1
}

echo "install/uninstall contract checks passed"
