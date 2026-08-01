#!/usr/bin/env bash
# Build the OmniDrive APK without Gradle.
#
#   bash android/build-apk.sh
#
# Uses the Android SDK's own tools directly — aapt2, javac, d8, zipalign,
# apksigner. That avoids downloading Gradle and the Android Gradle Plugin
# (~1 GB of caches) to compile two Java files, and it keeps the build
# reproducible from a plain SDK install.
#
# Override with: ANDROID_HOME, BUILD_TOOLS, PLATFORM, VERSION

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"

# shellcheck source=../version.sh
. "$root/version.sh"

# shellcheck source=../scripts/oauth-ldflags.sh
. "$root/scripts/oauth-ldflags.sh"
OAUTH_FLAGS="$(oauth_ldflags "$root/oauth.env")"
MIN_SDK=29
TARGET_SDK=36

# --- locate the SDK ---

SDK="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$HOME/AppData/Local/Android/Sdk}}"
[ -d "$SDK" ] || { echo "Android SDK not found. Set ANDROID_HOME." >&2; exit 1; }

pick_newest() { ls "$1" 2>/dev/null | sort -V | tail -1; }

BUILD_TOOLS="${BUILD_TOOLS:-$(pick_newest "$SDK/build-tools")}"
PLATFORM="${PLATFORM:-android-$TARGET_SDK}"
BT="$SDK/build-tools/$BUILD_TOOLS"
ANDROID_JAR="$SDK/platforms/$PLATFORM/android.jar"

[ -d "$BT" ]          || { echo "build-tools not found: $BT" >&2; exit 1; }
[ -f "$ANDROID_JAR" ] || { echo "android.jar not found: $ANDROID_JAR" >&2; exit 1; }

# Windows ships .exe wrappers; Linux and macOS do not.
suffix() { [ -f "$BT/$1$2" ] && echo "$BT/$1$2" || echo "$BT/$1"; }
AAPT2="$(suffix aapt2 .exe)"
ZIPALIGN="$(suffix zipalign .exe)"

# d8 and apksigner ship as .bat/.sh wrappers that mangle paths containing
# spaces on Windows. Their jars are right there, so invoke them directly.
D8_JAR="$BT/lib/d8.jar"
APKSIGNER_JAR="$BT/lib/apksigner.jar"
[ -f "$D8_JAR" ]        || { echo "d8.jar not found: $D8_JAR" >&2; exit 1; }
[ -f "$APKSIGNER_JAR" ] || { echo "apksigner.jar not found: $APKSIGNER_JAR" >&2; exit 1; }

# Git Bash hands POSIX paths to native Windows tools, which mostly works —
# except inside argument files, which nothing rewrites for us.
#
# Mixed-mode paths (C:/Users/Atanu Roy/...) rather than backslash form: javac
# argument files split on whitespace and treat a backslash as an escape, so
# every entry is also quoted.
if command -v cygpath >/dev/null 2>&1; then
  win() { cygpath -m "$1"; }
  win_list() { cygpath -m -f - | sed 's/.*/"&"/'; }
else
  win() { printf '%s' "$1"; }
  win_list() { sed 's/.*/"&"/'; }
fi

echo "SDK          $SDK"
echo "build-tools  $BUILD_TOOLS"
echo "platform     $PLATFORM"
echo

# --- 1. the server binaries, one per Android ABI ---
#
# Android only extracts and grants execute permission to files named lib*.so
# inside the APK's lib/<abi>/ directory. Naming the server binary
# libomnidrive.so is what makes it runnable at all on Android 10+.

echo "==> Building server binaries"
declare -A ALL_ABIS=(
  [arm64-v8a]="arm64"
  [armeabi-v7a]="arm"
  [x86_64]="amd64"
)

# Each ABI adds ~3 MB to the download. ABI=arm64-v8a produces a ~3.3 MB APK
# that covers every 64-bit phone, which is all of them since 2019.
declare -A ABIS=()
if [ -n "${ABI:-}" ]; then
  for abi in $ABI; do
    [ -n "${ALL_ABIS[$abi]:-}" ] || { echo "unknown ABI: $abi" >&2; exit 1; }
    ABIS[$abi]="${ALL_ABIS[$abi]}"
  done
else
  for abi in "${!ALL_ABIS[@]}"; do ABIS[$abi]="${ALL_ABIS[$abi]}"; done
fi

work="$here/.build"
rm -rf "$work"
mkdir -p "$work/lib" "$work/gen" "$work/classes"

for abi in "${!ABIS[@]}"; do
  goarch="${ABIS[$abi]}"
  mkdir -p "$work/lib/$abi"
  extra=()
  [ "$goarch" = "arm" ] && extra+=("GOARM=7")
  printf '    %-14s go=%s\n' "$abi" "$goarch"
  ( cd "$root" && env CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" "${extra[@]}" \
      go build -trimpath -ldflags "-s -w -X main.version=$VERSION$OAUTH_FLAGS" \
      -o "$work/lib/$abi/libomnidrive.so" ./cmd/omnidrive )
done
oauth_summary "$OAUTH_FLAGS"

# --- 2. resources ---

echo "==> Compiling resources"
"$AAPT2" compile --dir "$here/res" -o "$work/res.zip"

echo "==> Linking resources"
"$AAPT2" link \
  -o "$work/base.apk" \
  -I "$ANDROID_JAR" \
  --manifest "$here/AndroidManifest.xml" \
  -R "$work/res.zip" \
  --java "$work/gen" \
  --min-sdk-version "$MIN_SDK" \
  --target-sdk-version "$TARGET_SDK" \
  --version-code "$VERSION_CODE" \
  --version-name "$VERSION" \
  --auto-add-overlay

# --- 3. Java -> dex ---

echo "==> Compiling Java"
find "$here/java" "$work/gen" -name '*.java' | win_list > "$work/sources.txt"
# android.jar goes on the classpath rather than the bootclasspath: JDK 17
# rejects -bootclasspath with -target 17, and d8 only dexes our own classes
# anyway — the platform classes come from the device at runtime.
javac -nowarn -encoding UTF-8 \
  -source 17 -target 17 \
  -classpath "$ANDROID_JAR" \
  -d "$work/classes" \
  "@$(win "$work/sources.txt")"

echo "==> Dexing"
# Hand d8 a jar rather than a list of class files: one argument, no argument
# file, and therefore no path-quoting surprises.
jar --create --file "$work/classes.jar" -C "$work/classes" .
java -cp "$D8_JAR" com.android.tools.r8.D8 \
  --release --min-api "$MIN_SDK" --lib "$ANDROID_JAR" \
  --output "$work" "$work/classes.jar"

# --- 4. assemble ---
#
# aapt2 produces an APK containing only resources; the dex and native binaries
# have to be added afterwards. Done in Python because `zip` is not present on a
# stock Windows toolchain and we need per-entry control anyway.

echo "==> Assembling APK"
python - "$work" <<'PY'
import os, sys, shutil, zipfile

work = sys.argv[1]
base = os.path.join(work, "base.apk")
out  = os.path.join(work, "unsigned.apk")
shutil.copy(base, out)

with zipfile.ZipFile(out, "a", zipfile.ZIP_DEFLATED) as z:
    existing = set(z.namelist())

    dex = os.path.join(work, "classes.dex")
    z.write(dex, "classes.dex")

    lib = os.path.join(work, "lib")
    for abi in sorted(os.listdir(lib)):
        src = os.path.join(lib, abi, "libomnidrive.so")
        # extractNativeLibs="true" in the manifest means Android unpacks this
        # at install time, so storing it compressed is both allowed and a
        # significant download saving.
        z.write(src, "lib/%s/libomnidrive.so" % abi)

print("   entries:", len(zipfile.ZipFile(out).namelist()))
PY

# --- 5. align and sign ---

echo "==> Aligning"
"$ZIPALIGN" -p -f 4 "$work/unsigned.apk" "$work/aligned.apk"

KEYSTORE="$here/omnidrive-release.keystore"
STOREPASS="${KEYSTORE_PASS:-omnidrive}"

if [ ! -f "$KEYSTORE" ]; then
  echo "==> Creating a signing key (first run only)"
  # A self-signed key: Android requires every APK to be signed, and a
  # sideloaded app has no store to verify against. Keep this file — Android
  # refuses to upgrade an APK signed with a different key.
  keytool -genkeypair -v \
    -keystore "$(win "$KEYSTORE")" \
    -alias omnidrive \
    -keyalg RSA -keysize 4096 \
    -validity 10950 \
    -storepass "$STOREPASS" -keypass "$STOREPASS" \
    -dname "CN=OmniDrive, OU=Self-signed, O=OmniDrive, C=IN" >/dev/null 2>&1
fi

echo "==> Signing"
mkdir -p "$root/build"
APK="$root/build/omnidrive-$VERSION.apk"
java -jar "$APKSIGNER_JAR" sign \
  --ks "$(win "$KEYSTORE")" \
  --ks-key-alias omnidrive \
  --ks-pass "pass:$STOREPASS" \
  --key-pass "pass:$STOREPASS" \
  --min-sdk-version "$MIN_SDK" \
  --v1-signing-enabled true \
  --v2-signing-enabled true \
  --v3-signing-enabled true \
  --v4-signing-enabled false \
  --out "$(win "$APK")" \
  "$(win "$work/aligned.apk")"

java -jar "$APKSIGNER_JAR" verify --print-certs "$(win "$APK")" | head -4

rm -rf "$work"

echo
echo "  APK:  $APK"
echo "  ABIs: ${!ABIS[*]}"
python -c "import os,sys; print('  size: %.1f MB' % (os.path.getsize(sys.argv[1])/1048576))" "$APK"
echo
echo "  Install with:  adb install -r \"$APK\""
echo "  Or copy it to the phone and tap it."
