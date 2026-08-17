#!/usr/bin/env bash
# Install the "Live Record Refresh" CEP panel into Premiere Pro.
#
# Premiere has no menu for running a script, so a panel is the practical way to
# execute ExtendScript against the running application. The panel is unsigned,
# which means PlayerDebugMode has to be on or Premiere silently refuses to load
# it — that is the step people usually miss.
#
#   ./install-panel.sh          install (or update) and enable debug mode
#   ./install-panel.sh remove   uninstall
set -uo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/premiere-panel"
DEST="$HOME/Library/Application Support/Adobe/CEP/extensions/LiveRecordRefresh"

if [[ "${1:-}" == "remove" ]]; then
  rm -rf "$DEST" && echo "removed $DEST"
  exit 0
fi

if [[ ! -d "$SRC" ]]; then
  echo "panel source not found at $SRC" >&2
  exit 1
fi

mkdir -p "$DEST"
cp -R "$SRC/" "$DEST/"

# CSInterface.js is Adobe's bridge between the panel and ExtendScript. Premiere
# ships it, so copy it out of the host rather than vendoring a stale one.
CSI="$(find \
  "/Applications/Adobe Premiere Pro 2026/Adobe Premiere Pro 2026.app/Contents/CEP" \
  "/Library/Application Support/Adobe/CEP" \
  "$HOME/Library/Application Support/Adobe/CEP" \
  -name CSInterface.js 2>/dev/null | head -1)"

if [[ -n "$CSI" ]]; then
  cp "$CSI" "$DEST/CSInterface.js"
  echo "  CSInterface.js  from $(dirname "$CSI" | sed 's|.*/||')"
else
  echo "  WARNING: could not find CSInterface.js on this machine."
  echo "  Download it from https://github.com/Adobe-CEP/CEP-Resources"
  echo "  and drop it in: $DEST/CSInterface.js"
fi

# Unsigned extensions load only with PlayerDebugMode set. The CSXS version has
# moved over the years and Premiere reads whichever matches its runtime, so set
# the range rather than guessing which one this build uses.
for v in 9 10 11 12 13; do
  defaults write "com.adobe.CSXS.$v" PlayerDebugMode 1 2>/dev/null
done
defaults read com.adobe.CSXS.12 PlayerDebugMode >/dev/null 2>&1 \
  && echo "  PlayerDebugMode  enabled" \
  || echo "  PlayerDebugMode  could not be confirmed"

echo
echo "Installed to:"
echo "  $DEST"
echo
echo "Now, in Premiere:  Window > Extensions > Live Record Refresh"
echo "(Premiere must be restarted to pick up a newly installed panel.)"
