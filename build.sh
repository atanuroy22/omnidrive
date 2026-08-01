#!/usr/bin/env bash
# Cross-compile OmniDrive for phones and desktops.
#
#   ./build.sh              # every target
#   ./build.sh android      # just the Android binaries
#   ./build.sh android-arm64
#
# Everything is CGO-free and statically linked, so the output runs on any
# device of that architecture with no runtime, libc or package dependencies.

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=version.sh
. "$here/version.sh"

OUT="${OUT:-build}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/oauth-ldflags.sh
. "$here/scripts/oauth-ldflags.sh"
OAUTH_FLAGS="$(oauth_ldflags "$here/oauth.env")"

LDFLAGS="-s -w -X main.version=${VERSION}${OAUTH_FLAGS}"

# Android's Linux userspace runs ordinary static ELF binaries, so GOOS=linux is
# both correct and the most portable choice here — it needs no NDK.
declare -A TARGETS=(
  [android-arm64]="linux arm64"
  [android-arm]="linux arm"
  [android-x86_64]="linux amd64"
  [linux-amd64]="linux amd64"
  [linux-arm64]="linux arm64"
  [windows-amd64]="windows amd64"
  [darwin-arm64]="darwin arm64"
  [darwin-amd64]="darwin amd64"
)

build_one() {
  local name="$1"
  read -r goos goarch <<<"${TARGETS[$name]}"

  local out="${OUT}/omnidrive-${name}"
  [[ "$goos" == "windows" ]] && out+=".exe"

  local extra=()
  [[ "$goarch" == "arm" ]] && extra+=("GOARM=7")

  printf '  %-18s %s/%s\n' "$name" "$goos" "$goarch"
  env CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" "${extra[@]}" \
    go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/omnidrive
}

mkdir -p "$OUT"
echo "OmniDrive ${VERSION}"
oauth_summary "$OAUTH_FLAGS"

selection=("${@:-all}")
case "${selection[0]}" in
  all)
    for name in "${!TARGETS[@]}"; do build_one "$name"; done
    ;;
  android)
    for name in android-arm64 android-arm android-x86_64; do build_one "$name"; done
    ;;
  *)
    for name in "${selection[@]}"; do
      if [[ -z "${TARGETS[$name]:-}" ]]; then
        echo "unknown target: $name" >&2
        echo "available: ${!TARGETS[*]}" >&2
        exit 1
      fi
      build_one "$name"
    done
    ;;
esac

echo
ls -lh "$OUT" | tail -n +2
echo
echo "Copy the android-* binary to your phone and follow scripts/install-termux.sh"
