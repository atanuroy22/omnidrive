#!/data/data/com.termux/files/usr/bin/bash
# OmniDrive installer for Termux.
#
#   bash install-termux.sh                    # use a binary sitting next to this script
#   bash install-termux.sh ~/storage/downloads/omnidrive-android-arm64
#   OMNIDRIVE_URL=https://…/omnidrive-android-arm64 bash install-termux.sh
#
# Installs to $PREFIX/bin/omnidrive and creates an `omnidrive-start` helper that
# holds a wake lock so Android does not suspend the server.

set -euo pipefail

PREFIX="${PREFIX:-/data/data/com.termux/files/usr}"
BIN_DIR="$PREFIX/bin"
TARGET="$BIN_DIR/omnidrive"

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m error\033[0m %s\n' "$*" >&2; exit 1; }

[ -d "$PREFIX" ] || die "This script is for Termux. Install Termux from F-Droid (the Play Store build is abandoned)."

# --- work out which binary this device needs ---

case "$(uname -m)" in
  aarch64|arm64)          ARCH=arm64  ;;
  armv7l|armv8l|arm)      ARCH=arm    ;;
  x86_64|amd64)           ARCH=x86_64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac
ASSET="omnidrive-android-${ARCH}"
say "Device architecture: $(uname -m) → ${ASSET}"

SOURCE="${1:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ -z "$SOURCE" ]; then
  for candidate in "$SCRIPT_DIR/$ASSET" "$SCRIPT_DIR/../build/$ASSET" "./$ASSET"; do
    if [ -f "$candidate" ]; then SOURCE="$candidate"; break; fi
  done
fi

if [ -z "$SOURCE" ] && [ -n "${OMNIDRIVE_URL:-}" ]; then
  command -v curl >/dev/null || { say "Installing curl"; pkg install -y curl >/dev/null; }
  SOURCE="$TMPDIR/$ASSET"
  say "Downloading $OMNIDRIVE_URL"
  curl -fL --progress-bar -o "$SOURCE" "$OMNIDRIVE_URL"
fi

[ -n "$SOURCE" ] && [ -f "$SOURCE" ] || die "Could not find $ASSET.
Copy it into this folder, pass its path as an argument, or set OMNIDRIVE_URL.
If you have Go installed you can also build in place:  pkg install golang && go build ./cmd/omnidrive"

# --- install ---

# Termux's $PREFIX/bin is on the app's private storage, which is one of the few
# places Android still allows executing from. Never install to /sdcard: it is
# mounted noexec on every modern device.
say "Installing to $TARGET"
mkdir -p "$BIN_DIR"
cat "$SOURCE" > "$TARGET"   # copy contents so a busy binary can be replaced
chmod 700 "$TARGET"

"$TARGET" version || die "The installed binary does not run on this device."

# --- start helper ---

cat > "$BIN_DIR/omnidrive-start" <<'LAUNCHER'
#!/data/data/com.termux/files/usr/bin/bash
# Start OmniDrive with a wake lock so Android's Doze mode leaves it alone.
set -euo pipefail

if command -v termux-wake-lock >/dev/null; then
  termux-wake-lock
  trap 'termux-wake-unlock 2>/dev/null || true' EXIT
fi

# Android has no /etc/resolv.conf. The server cannot ask for the nameservers
# itself — os/exec probes with faccessat2(2), which Android's seccomp policy
# turns into a fatal SIGSYS — but a shell may call getprop safely, so hand the
# answer over through the environment. Without this the server still works via
# public resolvers; with it, the network's own servers are tried first.
if [ -z "${OMNIDRIVE_DNS:-}" ] && command -v getprop >/dev/null; then
  dns=""
  for prop in net.dns1 net.dns2 net.dns3 net.dns4; do
    value="$(getprop "$prop" 2>/dev/null || true)"
    [ -n "$value" ] && dns="${dns:+$dns,}$value"
  done
  [ -n "$dns" ] && export OMNIDRIVE_DNS="$dns"
fi

exec omnidrive "$@"
LAUNCHER
chmod 700 "$BIN_DIR/omnidrive-start"

# --- guidance the user actually needs ---

cat <<EOF

  OmniDrive is installed.

  Start it:        omnidrive-start
  Then open:       http://127.0.0.1:8787

  Useful:
    omnidrive accounts              list connected drives
    omnidrive pair                  send this setup to another device
    omnidrive join <url> <code>     receive a setup from another device

EOF

if ! command -v termux-wake-lock >/dev/null; then
  warn "termux-api is not installed, so the server may be suspended when the screen locks."
  warn "Fix with:  pkg install termux-api   (and install the Termux:API app from F-Droid)"
fi

if [ ! -d "$HOME/storage" ]; then
  warn "Run 'termux-setup-storage' if you want downloads saved to your Downloads folder."
fi

cat <<'EOF'
  Android 12 and newer kill background processes aggressively. If the server
  stops when you switch apps, keep the Termux notification pinned ("Acquire
  wakelock" from the notification shade), and exclude Termux from battery
  optimisation in Android settings.

EOF
