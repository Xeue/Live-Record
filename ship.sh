#!/usr/bin/env bash
# Build, bundle, sign and package Live Record as a distributable .dmg.
#
#   ./ship.sh              build -> bundle GStreamer -> sign -> .dmg
#   ./ship.sh notarize     ...and notarise + staple the .app AND the .dmg
#
# Output: build/dist/LiveRecord-<version>-macos-arm64.dmg
#
# The .app is SELF-CONTAINED: it carries its own GStreamer plugins, their
# dependent libraries, the plugin scanner and the command-line tools. A Mac with
# no Homebrew runs it. That is checked, not assumed — stage 4 fails the build if
# a single load command still points at a Homebrew prefix.
#
# Several details here were learned the hard way by an in-house sibling project
# and are reproduced deliberately. They are commented where they occur, because
# each looks like fussiness and is not:
#   - the DMG volume name must contain no spaces
#   - signing walks the bundle rather than a list of directories
#   - find writes to a temp file, never a process substitution
#   - signing retries, because Apple's timestamp service flakes
#   - the .app and the .dmg are notarised separately
set -uo pipefail
cd "$(dirname "$0")"
export PATH="$PATH:$HOME/go/bin:/opt/homebrew/bin"
export PKG_CONFIG_PATH="/opt/homebrew/lib/pkgconfig:${PKG_CONFIG_PATH:-}"

VERSION="$(python3 -c 'import json;print(json.load(open("wails.json"))["info"]["productVersion"])')"
APP="build/bin/liverecord.app"
DIST="build/dist"
ENTITLEMENTS="build/darwin/entitlements.plist"
NOTARY_PROFILE="${LIVERECORD_NOTARY_PROFILE:-sygnal-notary}"
DMG="$DIST/LiveRecord-$VERSION-macos-arm64.dmg"

fail() { echo "" >&2; echo "ship.sh: $*" >&2; exit 1; }
step() { echo ""; echo "──── $*"; }

IDENTITY="${LIVERECORD_IDENTITY:-$(security find-identity -v -p codesigning \
  | grep "Developer ID Application" | head -1 | sed -E 's/.*"(.*)"/\1/')}"
[ -n "$IDENTITY" ] || fail "no 'Developer ID Application' certificate in the keychain."

for t in go wails gst-inspect-1.0 otool install_name_tool codesign hdiutil ditto; do
  command -v "$t" >/dev/null || fail "$t not found on PATH."
done

echo "Live Record $VERSION"
echo "  identity   $IDENTITY"
echo "  output     $DMG"

# ── 1. build ────────────────────────────────────────────────────────────────
step "1. build"
rm -rf "$APP"
wails build -tags desktop -clean >/dev/null || fail "wails build failed."
[ -d "$APP" ] || fail "no app bundle at $APP."
echo "  built $APP"

# ── 2. bundle GStreamer ─────────────────────────────────────────────────────
#
# Elements -> plugins -> transitive dylib closure. Derived from the app's own
# requiredElements list rather than hand-maintained: a hand list is correct only
# until someone adds an element, and the failure is not a build error but a
# missing plugin on a customer's Mac.
step "2. bundle GStreamer"
FRAMEWORKS="$APP/Contents/Frameworks"
PLUGINDIR="$APP/Contents/Resources/gstreamer-1.0"
MACOSDIR="$APP/Contents/MacOS"
mkdir -p "$FRAMEWORKS" "$PLUGINDIR"

ELEMENTS="$(python3 - <<'PY'
import re
src = open("app.go").read()
i = src.index("var requiredElements")
print("\n".join(re.findall(r'"([a-z0-9_]+)"', src[i:src.index("}", i)])))
PY
)"
[ -n "$ELEMENTS" ] || fail "could not read requiredElements from app.go."

# The tools the app itself execs: preflight runs gst-inspect, and the subprocess
# engine runs gst-launch.
for tool in gst-inspect-1.0 gst-launch-1.0; do
  cp -f "$(command -v $tool)" "$MACOSDIR/" || fail "could not copy $tool."
done
SCANNER="$(/opt/homebrew/bin/gst-inspect-1.0 --version >/dev/null 2>&1; \
  find /opt/homebrew/Cellar/gstreamer -name gst-plugin-scanner -type f 2>/dev/null | head -1)"
[ -n "$SCANNER" ] || fail "gst-plugin-scanner not found."
cp -f "$SCANNER" "$MACOSDIR/gst-plugin-scanner"

PLUGINS=""
for el in $ELEMENTS; do
  so="$(gst-inspect-1.0 "$el" 2>/dev/null | awk '/Filename/{print $2; exit}')"
  [ -n "$so" ] || fail "element '$el' not found — cannot bundle it."
  PLUGINS="$PLUGINS$so"$'\n'
done
PLUGINS="$(printf %s "$PLUGINS" | sort -u | grep .)"
echo "  $(printf %s "$PLUGINS" | wc -l | tr -d ' ') plugins for $(echo $ELEMENTS | wc -w | tr -d ' ') elements"
while read -r so; do cp -f "$so" "$PLUGINDIR/"; done <<< "$PLUGINS"

# Emits "referenced-name|source-path" pairs.
#
# The referenced name is NOT the file's real name, and that distinction is the
# whole point. Homebrew ships libsrt.1.5.dylib as a symlink to libsrt.1.5.4.dylib,
# and a load command names the SYMLINK. Resolving to the real path and copying
# under that basename puts libsrt.1.5.4.dylib in the bundle while every load
# command still asks for libsrt.1.5.dylib — measured, and it fails at dlopen on
# exactly the machine this bundle exists for, the one with no Homebrew to fall
# back to.
resolve_deps() {
  python3 - "$@" <<'PYDEPS'
import subprocess, sys, os, re

pairs, seen, queue = {}, set(), list(sys.argv[1:])
EXTERNAL = ("/opt/homebrew", "/usr/local/Cellar", "/usr/local/opt")

def load_commands(p):
    try:
        t = subprocess.run(["otool", "-l", p], capture_output=True, text=True).stdout
    except Exception:
        return [], []
    return (re.findall(r"name (\S+) \(offset", t),
            re.findall(r"path (\S+) \(offset", t))

while queue:
    cur = queue.pop()
    if cur in seen:
        continue
    seen.add(cur)
    loads, rpaths = load_commands(cur)
    for dep in loads:
        cand = dep
        if dep.startswith("@rpath/"):
            for rp in rpaths:
                t = os.path.join(rp, dep[len("@rpath/"):])
                if os.path.exists(t):
                    cand = t
                    break
        if not cand.startswith(EXTERNAL):
            continue
        ref = os.path.basename(cand)      # the name that is referenced
        src = os.path.realpath(cand)      # where the bytes actually are
        if ref not in pairs:
            pairs[ref] = src
            queue.append(src)

for ref in sorted(pairs):
    print(ref + "|" + pairs[ref])
PYDEPS
}
ROOTS=("$MACOSDIR/Live Record" "$MACOSDIR/gst-inspect-1.0" "$MACOSDIR/gst-launch-1.0" \
       "$MACOSDIR/gst-plugin-scanner")
while read -r so; do ROOTS+=("$PLUGINDIR/$(basename "$so")"); done <<< "$PLUGINS"
DEPS="$(resolve_deps "${ROOTS[@]}")"
[ -n "$DEPS" ] || fail "dependency walk produced nothing — otool parsing is wrong."
echo "  $(printf %s "$DEPS" | wc -l | tr -d ' ') dependent libraries"
while IFS='|' read -r ref src; do
  [ -n "$ref" ] && cp -f "$src" "$FRAMEWORKS/$ref"
done <<< "$DEPS"
chmod u+w "$FRAMEWORKS"/*.dylib "$PLUGINDIR"/*.so "$MACOSDIR"/gst-* 2>/dev/null

# ── 3. rewrite load commands ────────────────────────────────────────────────
step "3. rewrite load commands"
rewrite() {
  local f="$1" base rel
  case "$f" in
    "$FRAMEWORKS"/*) rel="@loader_path" ;;
    "$PLUGINDIR"/*)  rel="@loader_path/../../Frameworks" ;;
    *)               rel="@executable_path/../Frameworks" ;;
  esac
  install_name_tool -id "$rel/$(basename "$f")" "$f" 2>/dev/null
  otool -l "$f" 2>/dev/null | grep -oE "name /(opt/homebrew|usr/local)/\S+" | sed 's/^name //' \
  | sort -u | while read -r dep; do
      install_name_tool -change "$dep" "$rel/$(basename "$dep")" "$f" 2>/dev/null
    done
}
find "$FRAMEWORKS" "$PLUGINDIR" "$MACOSDIR" -type f > /tmp/lr_macho.txt
while read -r f; do
  file "$f" 2>/dev/null | grep -q "Mach-O" && rewrite "$f"
done < /tmp/lr_macho.txt
echo "  rewritten"

# ── 4. prove it is self-contained ───────────────────────────────────────────
#
# The check that makes the claim honest. A single missed load command is a crash
# on a machine that has no Homebrew, and it will not reproduce here.
step "4. verify self-contained"
LEAKS="$(while read -r f; do
  file "$f" 2>/dev/null | grep -q "Mach-O" || continue
  otool -L "$f" 2>/dev/null | grep -E "/opt/homebrew|/usr/local/(Cellar|opt)" \
    | sed "s|^|$(basename "$f"): |"
done < /tmp/lr_macho.txt)"
if [ -n "$LEAKS" ]; then
  echo "$LEAKS" | head -20 >&2
  fail "$(printf %s "$LEAKS" | wc -l | tr -d ' ') load command(s) still point outside the bundle."
fi
echo "  zero Homebrew load commands in the bundle"

# ── 5. sign ─────────────────────────────────────────────────────────────────
#
# Inner Mach-Os first, then the bundle. --deep is not used: it is a verification
# flag, and as a signing flag it hides what it did and did not reach.
#
# find into a temp FILE, never a process substitution: codesign --force re-signs
# via a <name>.cstemp rename, and a find still walking a directory an earlier
# iteration is signing can hand that transient file to codesign.
#
# Retries because the secure timestamp is a live call to Apple's timestamp
# authority, once per Mach-O — hundreds here — and it flakes. Dropping the
# timestamp instead would make every signature expire with the certificate.
step "5. sign"
sign_one() {
  local target="$1" attempt
  for attempt in 1 2 3; do
    codesign --force --options runtime --timestamp \
      --entitlements "$ENTITLEMENTS" --sign "$IDENTITY" "$target" >/dev/null 2>&1 && return 0
    echo "  timestamp/sign attempt $attempt failed for $(basename "$target") — retrying in 5s" >&2
    sleep 5
  done
  return 1
}
N=0
while read -r f; do
  file "$f" 2>/dev/null | grep -q "Mach-O" || continue
  sign_one "$f" || fail "could not sign $f"
  N=$((N+1))
done < /tmp/lr_macho.txt
sign_one "$APP" || fail "could not sign the app bundle."
echo "  signed $N inner Mach-O files, then the bundle"

step "6. verify signature"
codesign --verify --deep --strict --verbose=2 "$APP" 2>&1 | sed 's/^/  /' \
  || fail "signature verification failed."
codesign -dv --verbose=2 "$APP" 2>&1 \
  | grep -E "^(Identifier|Authority|TeamIdentifier|Timestamp|Runtime)" | sed 's/^/  /'

# ── 7. notarise the .app ────────────────────────────────────────────────────
#
# The .app and the .dmg are notarised SEPARATELY. Stapling only the image puts
# the ticket on the image — drag the app out and the copy on disk has no ticket
# of its own, so Gatekeeper has to reach Apple to assess it. On a venue network,
# or none, that is a first launch that hangs and then refuses.
#
# What is SUBMITTED and what is STAPLED are different paths for an .app: an .app
# must be zipped to upload, and stapling a zip fails outright.
notarise() {
  local what="$1" submit="$2" staple="$3" out status
  echo "  submitting $(basename "$submit") — minutes, and waiting is the point"
  out="$(xcrun notarytool submit "$submit" --keychain-profile "$NOTARY_PROFILE" --wait 2>&1)" \
    || { echo "$out" >&2; fail "notarytool submit failed for $what."; }
  echo "$out" | sed 's/^/    /'
  status="$(echo "$out" | sed -n 's/^ *status: \(.*\)$/\1/p' | tail -1)"
  [ "$status" = "Accepted" ] || fail "$what was not accepted: $status"
  xcrun stapler staple "$staple" | sed 's/^/    /' || fail "stapling $what failed."
}

mkdir -p "$DIST"
if [ "${1:-}" = "notarize" ]; then
  step "7. notarise the app"
  if ! xcrun notarytool history --keychain-profile "$NOTARY_PROFILE" >/dev/null 2>&1; then
    fail "no notarytool keychain profile '$NOTARY_PROFILE'. Store one with:
  xcrun notarytool store-credentials $NOTARY_PROFILE \\
    --apple-id <your-apple-id> --team-id <team> --password <app-specific-password>"
  fi
  ZIP="$DIST/liverecord-$VERSION-notarize.zip"
  rm -f "$ZIP"
  ditto -c -k --keepParent "$APP" "$ZIP"
  notarise "the .app" "$ZIP" "$APP"
  rm -f "$ZIP"
else
  step "7. notarise the app — SKIPPED"
  echo "  run ./ship.sh notarize to notarise. Without it, Gatekeeper blocks the"
  echo "  app on any Mac other than this one."
fi

# ── 8. disk image ───────────────────────────────────────────────────────────
#
# A .dmg, not a .pkg: this installs nothing and registers no service, so an
# installer with an admin prompt would be theatre. The /Applications symlink is
# the drag-and-drop convention.
#
# THE VOLUME NAME CONTAINS NO SPACES. With the symlink present, a spaced volname
# makes hdiutil fail with "Operation not permitted", which reads like a
# permissions or TCC problem and is not one. Hyphens are reliable.
step "8. disk image"
STAGE="$(mktemp -d)"
cp -R "$APP" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
rm -f "$DMG"
hdiutil create -volname "LiveRecord-$VERSION" -srcfolder "$STAGE" \
  -fs HFS+ -format UDZO -ov "$DMG" >/dev/null || fail "hdiutil create failed."
rm -rf "$STAGE"
sign_one_dmg() {
  codesign --force --timestamp --sign "$IDENTITY" "$DMG" >/dev/null 2>&1
}
sign_one_dmg || fail "could not sign the disk image."
echo "  $DMG ($(du -h "$DMG" | cut -f1))"

if [ "${1:-}" = "notarize" ]; then
  step "9. notarise the disk image"
  notarise "the .dmg" "$DMG" "$DMG"
  echo ""
  echo "Gatekeeper:"
  spctl -a -vvv -t open --context context:primary-signature "$DMG" 2>&1 | sed 's/^/  /' || true
fi

rm -f /tmp/lr_macho.txt
echo ""
echo "Done: $DMG"
