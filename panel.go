package main

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The panel is embedded so the app can install it without needing the source
// tree — a packaged .app has no repository beside it.
//
//go:embed premiere-panel
var panelFS embed.FS

const (
	panelDirName = "LiveRecordRefresh"
	panelVersion = "1.0.0"
)

// PanelStatus describes what is installed and whether Premiere could load it.
type PanelStatus struct {
	Installed  bool   `json:"installed"`
	Version    string `json:"version"`    // version currently installed
	Latest     string `json:"latest"`     // version this app ships
	Path       string `json:"path"`       // where it goes
	DebugMode  bool   `json:"debugMode"`  // unsigned panels need this
	HasBridge  bool   `json:"hasBridge"`  // CSInterface.js present
	PremierePath string `json:"premierePath"`
	Note       string `json:"note"`
}

func panelInstallDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Application Support", "Adobe", "CEP",
		"extensions", panelDirName)
}

// findPremiere returns the newest installed Premiere Pro application bundle.
func findPremiere() string {
	matches, _ := filepath.Glob("/Applications/Adobe Premiere Pro */Adobe Premiere Pro *.app")
	best := ""
	for _, m := range matches {
		if m > best { // names sort by year, so the highest is the newest
			best = m
		}
	}
	return best
}

// findCSInterface locates Adobe's panel-to-host bridge. It is not embedded:
// it belongs to the host application and should match the installed version,
// so it is copied out of Premiere rather than vendored.
func findCSInterface() string {
	roots := []string{}
	if p := findPremiere(); p != "" {
		roots = append(roots, filepath.Join(p, "Contents", "CEP"))
	}
	home, _ := os.UserHomeDir()
	roots = append(roots,
		"/Library/Application Support/Adobe/CEP",
		filepath.Join(home, "Library", "Application Support", "Adobe", "CEP"))

	found := ""
	for _, root := range roots {
		if found != "" {
			break
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable corner of the tree; keep looking
			}
			if !d.IsDir() && d.Name() == "CSInterface.js" {
				found = p
				return filepath.SkipAll
			}
			return nil
		})
	}
	return found
}

// debugModeEnabled reports whether unsigned extensions are allowed to load.
func debugModeEnabled() bool {
	for _, v := range csxsVersions {
		out, err := exec.Command("defaults", "read", "com.adobe.CSXS."+v, "PlayerDebugMode").Output()
		if err == nil && strings.TrimSpace(string(out)) == "1" {
			return true
		}
	}
	return false
}

// Premiere reads whichever CSXS domain matches its runtime, and that has moved
// over the years, so all the plausible ones are set rather than guessed at.
var csxsVersions = []string{"9", "10", "11", "12", "13"}

func installedPanelVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "CSXS", "manifest.xml"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `xml:"ExtensionBundleVersion,attr"`
	}
	if err := xml.Unmarshal(b, &m); err != nil {
		return "unknown"
	}
	return m.Version
}

// PanelState reports what is installed right now.
func PanelState() PanelStatus {
	dir := panelInstallDir()
	st := PanelStatus{
		Path:         dir,
		Latest:       panelVersion,
		PremierePath: findPremiere(),
		DebugMode:    debugModeEnabled(),
	}
	if _, err := os.Stat(filepath.Join(dir, "CSXS", "manifest.xml")); err == nil {
		st.Installed = true
		st.Version = installedPanelVersion(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "CSInterface.js")); err == nil {
		st.HasBridge = true
	}

	switch {
	case st.PremierePath == "":
		st.Note = "Premiere Pro was not found in /Applications."
	case st.Installed && st.Version != st.Latest:
		st.Note = "An older version is installed. Install again to update it."
	case st.Installed && !st.HasBridge:
		st.Note = "Installed, but Adobe's CSInterface.js is missing — reinstall."
	case st.Installed:
		st.Note = "Installed. In Premiere: Window > Extensions > Live Record Refresh."
	default:
		st.Note = "Not installed."
	}
	return st
}

// InstallPanel copies the embedded panel into Premiere's user extensions folder
// and enables the setting that lets an unsigned panel load.
func InstallPanel() (PanelStatus, error) {
	dir := panelInstallDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return PanelState(), fmt.Errorf("could not create %s: %w", dir, err)
	}

	// Copy the embedded tree, stripping the leading "premiere-panel/".
	err := fs.WalkDir(panelFS, "premiere-panel", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("premiere-panel", p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := panelFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return PanelState(), fmt.Errorf("copying the panel: %w", err)
	}

	// Adobe's bridge comes from the host, not from us.
	if src := findCSInterface(); src != "" {
		if b, err := os.ReadFile(src); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "CSInterface.js"), b, 0o644)
		}
	}

	// Without this Premiere silently refuses to load an unsigned extension —
	// no error, no panel in the menu, which is the confusing part.
	for _, v := range csxsVersions {
		_ = exec.Command("defaults", "write", "com.adobe.CSXS."+v, "PlayerDebugMode", "1").Run()
	}

	st := PanelState()
	if !st.HasBridge {
		return st, fmt.Errorf("panel installed, but Adobe's CSInterface.js could not be found " +
			"on this machine — the panel will not be able to talk to Premiere")
	}
	return st, nil
}

// RemovePanel deletes the installed panel. PlayerDebugMode is left alone: other
// extensions may rely on it, and turning it off is not ours to decide.
func RemovePanel() (PanelStatus, error) {
	dir := panelInstallDir()
	if err := os.RemoveAll(dir); err != nil {
		return PanelState(), err
	}
	return PanelState(), nil
}

// ---------------------------------------------------------------------------
// portable installer
//
// The recorder runs on the record machine; the editor is usually somewhere
// else. This builds a zip that installs the panel on a machine that has never
// seen this app — so it carries its own installer, and finds that machine's own
// CSInterface.js rather than shipping Adobe's file around.

const panelInstallerScript = `#!/usr/bin/env bash
# Install the "Live Record Refresh" panel into Adobe Premiere Pro.
#
# Double-click this file, or run it from Terminal. It does three things:
#   1. copies the panel into your user extensions folder
#   2. copies Adobe's CSInterface.js out of your own Premiere installation
#   3. enables PlayerDebugMode, without which Premiere silently refuses to load
#      an unsigned panel — no error, no panel in the menu
set -uo pipefail
cd "$(dirname "$0")"

DEST="$HOME/Library/Application Support/Adobe/CEP/extensions/LiveRecordRefresh"

echo "Live Record Refresh — Premiere panel installer"
echo

if [[ ! -d panel ]]; then
  echo "ERROR: 'panel' folder missing. Unzip the whole archive and run this again." >&2
  read -n1 -rsp $'\nPress any key to close…\n'; exit 1
fi

PPRO="$(ls -d /Applications/Adobe\ Premiere\ Pro\ */Adobe\ Premiere\ Pro\ *.app 2>/dev/null | tail -1)"
if [[ -z "$PPRO" ]]; then
  echo "ERROR: Premiere Pro was not found in /Applications." >&2
  read -n1 -rsp $'\nPress any key to close…\n'; exit 1
fi
echo "  Premiere:  $(basename "$PPRO")"

mkdir -p "$DEST"
cp -R panel/ "$DEST/"
echo "  Installed: $DEST"

CSI="$(find "$PPRO/Contents/CEP" "/Library/Application Support/Adobe/CEP" \
        "$HOME/Library/Application Support/Adobe/CEP" \
        -name CSInterface.js 2>/dev/null | head -1)"
if [[ -n "$CSI" ]]; then
  cp "$CSI" "$DEST/CSInterface.js"
  echo "  Bridge:    CSInterface.js copied from Premiere"
else
  echo "  WARNING:   CSInterface.js not found — the panel cannot talk to Premiere."
  echo "             Get it from https://github.com/Adobe-CEP/CEP-Resources"
  echo "             and put it in: $DEST/"
fi

for v in 9 10 11 12 13; do
  defaults write "com.adobe.CSXS.$v" PlayerDebugMode 1 2>/dev/null
done
echo "  Unsigned panels: enabled"

echo
echo "Done. Restart Premiere, then open:"
echo "  Window > Extensions > Live Record Refresh"
echo
read -n1 -rsp $'Press any key to close…\n'
`

const panelReadme = `Live Record Refresh — Premiere Pro panel
========================================

WHAT IT IS
  Premiere only treats a growing file as growing media if its importer says it
  supports that. A growing MXF qualifies; a growing ProRes QuickTime does not —
  so the clip never goes italic, the "Refresh growing Files Every N seconds"
  setting never fires for it, and the Source Monitor's Force Media Refresh
  button does nothing.

  This panel calls refreshMedia() instead, which is a different code path and
  does extend the clip. It works out which clips are still being written and
  refreshes only those.

INSTALL
  Double-click install.command.
  If macOS blocks it: right-click > Open, or run  bash install.command

  Then restart Premiere and open:
    Window > Extensions > Live Record Refresh

USE
  Set an interval, press Start, leave it. Growing clips are marked with a red
  dot; the Gained column shows how much each has picked up.

REMOVE
  Delete this folder:
    ~/Library/Application Support/Adobe/CEP/extensions/LiveRecordRefresh
`

// BuildPanelInstaller returns a zip that installs the panel on another machine.
func BuildPanelInstaller() ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	add := func(name string, mode os.FileMode, body []byte) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write(body)
		return err
	}

	if err := add("LiveRecordRefresh/README.txt", 0o644, []byte(panelReadme)); err != nil {
		return nil, err
	}
	// Executable, so double-clicking it in Finder runs it.
	if err := add("LiveRecordRefresh/install.command", 0o755, []byte(panelInstallerScript)); err != nil {
		return nil, err
	}

	err := fs.WalkDir(panelFS, "premiere-panel", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel("premiere-panel", p)
		if err != nil {
			return err
		}
		b, err := panelFS.ReadFile(p)
		if err != nil {
			return err
		}
		return add("LiveRecordRefresh/panel/"+filepath.ToSlash(rel), 0o644, b)
	})
	if err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
