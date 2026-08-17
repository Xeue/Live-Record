//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Bundled GStreamer.
//
// A .app that has to work on a Mac without Homebrew carries its own GStreamer:
// plugins in Contents/Resources/gstreamer-1.0, their dependent dylibs in
// Contents/Frameworks, and the scanner and command-line tools in Contents/MacOS.
// ship.sh puts them there and rewrites every load command.
//
// GStreamer finds none of that by itself. It reads the environment, and it reads
// it when gst_init() runs — so this has to be applied BEFORE the first GStreamer
// call, and before any gst-inspect child is spawned. Get the ordering wrong and
// the registry is built from Homebrew's paths on the developer's machine and
// from nothing at all on the customer's.

var gstEnvOnce sync.Once

// bundleResources returns Contents/Resources when running from a .app, else "".
func bundleResources() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	// .../Foo.app/Contents/MacOS/exe -> .../Foo.app/Contents/Resources
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" {
		return ""
	}
	return filepath.Join(contents, "Resources")
}

// bundledGStreamer reports the plugin directory if this build carries its own
// GStreamer, else "".
func bundledGStreamer() string {
	res := bundleResources()
	if res == "" {
		return ""
	}
	dir := filepath.Join(res, "gstreamer-1.0")
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir
	}
	return ""
}

// setupGStreamerEnv points GStreamer at the bundled runtime, if there is one.
// Safe to call repeatedly; only the first call does anything.
func setupGStreamerEnv() {
	gstEnvOnce.Do(func() {
		plugins := bundledGStreamer()
		if plugins == "" {
			return // developer machine: Homebrew's own defaults are correct
		}
		contents := filepath.Dir(filepath.Dir(plugins)) // .../Contents
		macos := filepath.Join(contents, "MacOS")

		// Both spellings. GStreamer consults PATH and SYSTEM_PATH separately, and
		// a stale one inherited from the environment would let Homebrew plugins
		// load beside ours — mismatched ABI, and impossible to diagnose from a
		// crash report.
		set := map[string]string{
			"GST_PLUGIN_PATH":          plugins,
			"GST_PLUGIN_SYSTEM_PATH":   plugins,
			"GST_PLUGIN_PATH_1_0":      plugins,
			"GST_PLUGIN_SYSTEM_PATH_1_0": plugins,
			"GST_PLUGIN_SCANNER":       filepath.Join(macos, "gst-plugin-scanner"),
			"GST_PLUGIN_SCANNER_1_0":   filepath.Join(macos, "gst-plugin-scanner"),

			// The registry must be per-user and writable. Inside the bundle it
			// would be read-only, and GStreamer would rescan every plugin on
			// every launch — seconds of startup, every time.
			"GST_REGISTRY_1_0": filepath.Join(userStateDir(), "registry.bin"),

			// ORC's JIT is disabled rather than entitled here: the entitlement
			// covers our own process, but a spawned gst-inspect inherits the
			// hardened runtime without inheriting a reason to be trusted.
			// The C fallbacks are slower and always work.
			"ORC_CODE": "backup",
		}
		for k, v := range set {
			_ = os.Setenv(k, v)
		}
		// Drop anything pointing at a Homebrew prefix we are no longer using.
		for _, k := range []string{"DYLD_LIBRARY_PATH", "DYLD_FALLBACK_LIBRARY_PATH"} {
			if v := os.Getenv(k); strings.Contains(v, "/opt/homebrew") ||
				strings.Contains(v, "/usr/local/Cellar") {
				_ = os.Unsetenv(k)
			}
		}
	})
}

// userStateDir is a writable place for the plugin registry.
func userStateDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		p := filepath.Join(dir, "LiveRecord")
		if os.MkdirAll(p, 0o755) == nil {
			return p
		}
	}
	return os.TempDir()
}
