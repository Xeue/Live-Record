#!/usr/bin/env bash
# Build and sign Live Record.app with a Developer ID certificate.
#
#   ./sign.sh                 build, sign, verify
#   ./sign.sh notarize        ...then submit to Apple and staple the ticket
#
# Notarisation needs credentials this script will not invent. Store them once:
#   xcrun notarytool store-credentials liverecord \
#     --apple-id you@example.com --team-id 5P76UVY5WF --password <app-specific-password>
#
# The app is signed with the hardened runtime, which notarisation requires, plus
# the entitlements in build/darwin/entitlements.plist. Those are not optional:
# GStreamer comes from Homebrew and dlopen()s a plugin per element, and library
# validation would refuse all of it.
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$PATH:$HOME/go/bin"

APP="build/bin/liverecord.app"
ENTITLEMENTS="build/darwin/entitlements.plist"
KEYCHAIN_PROFILE="liverecord"

IDENTITY="${LIVERECORD_IDENTITY:-$(security find-identity -v -p codesigning \
  | grep "Developer ID Application" | head -1 \
  | sed -E 's/.*"(.*)"/\1/')}"

if [[ -z "$IDENTITY" ]]; then
  echo "No 'Developer ID Application' certificate found in the keychain." >&2
  echo "Set LIVERECORD_IDENTITY to choose a different one." >&2
  exit 1
fi

echo "Identity:  $IDENTITY"
echo

echo "Building…"
wails build -tags desktop -clean >/dev/null
[[ -d "$APP" ]] || { echo "build produced no app at $APP" >&2; exit 1; }

echo "Signing…"
# Sign inner code first, then the bundle: --deep is deprecated and does not
# apply entitlements to nested binaries correctly.
find "$APP/Contents" -type f \( -perm -u+x -o -name "*.dylib" \) 2>/dev/null \
  | while read -r f; do
      codesign --force --options runtime --timestamp \
        --entitlements "$ENTITLEMENTS" --sign "$IDENTITY" "$f" 2>/dev/null || true
    done

codesign --force --options runtime --timestamp \
  --entitlements "$ENTITLEMENTS" --sign "$IDENTITY" "$APP"

echo
echo "Verifying…"
codesign --verify --deep --strict --verbose=2 "$APP" 2>&1 | sed 's/^/  /'
echo
codesign -dv --verbose=4 "$APP" 2>&1 \
  | grep -E "^(Identifier|Authority|TeamIdentifier|Timestamp|Runtime|Signature)" | sed 's/^/  /'
echo
# Gatekeeper's own opinion. Before notarisation this reports "rejected" with
# "not notarized" — that is expected, and is what `./sign.sh notarize` fixes.
echo "Gatekeeper:"
spctl -a -vvv -t exec "$APP" 2>&1 | sed 's/^/  /' || true

if [[ "${1:-}" == "notarize" ]]; then
  echo
  echo "Notarising…"
  ZIP="build/bin/liverecord-notarize.zip"
  rm -f "$ZIP"
  ditto -c -k --keepParent "$APP" "$ZIP"
  xcrun notarytool submit "$ZIP" --keychain-profile "$KEYCHAIN_PROFILE" --wait
  echo "Stapling…"
  xcrun stapler staple "$APP"
  xcrun stapler validate "$APP" | sed 's/^/  /'
  rm -f "$ZIP"
  echo
  spctl -a -vvv -t exec "$APP" 2>&1 | sed 's/^/  /' || true
fi

echo
echo "Signed: $APP"
