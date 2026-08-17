package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// insideAppBundle reports whether this executable is running from a .app.
//
// Writability is not the test to use here. A bundle sitting in a user's own
// folder is perfectly writable, so a writability check happily drops
// config.json inside Contents/MacOS — invisible to the operator, wiped by the
// next build, and enough to invalidate the code signature.
func insideAppBundle() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return strings.Contains(filepath.ToSlash(exe), ".app/Contents/MacOS/")
}

// settingsPath is where config.json lives: beside the binary for a command-line
// run, and in Application Support for the packaged app.
func settingsPath() string {
	if !insideAppBundle() {
		if exe, err := os.Executable(); err == nil {
			return filepath.Join(filepath.Dir(exe), "config.json")
		}
		if wd, err := os.Getwd(); err == nil {
			return filepath.Join(wd, "config.json")
		}
		return "config.json"
	}
	dir, err := os.UserConfigDir() // ~/Library/Application Support
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Library", "Application Support")
	}
	return filepath.Join(dir, "Live Record", "config.json")
}

func openInFinder(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	return exec.Command("open", path).Run()
}
