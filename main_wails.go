//go:build dev || production || bindings

// main_wails.go is the native macOS entry point, built by the Wails CLI.
//
// # Why the startup work is all inside OnStartup
//
// Under the `bindings` tag the Wails CLI runs main() only to dump the binding
// definitions and exit — OnStartup is never called. Anything done before
// wails.Run would therefore start recording pipelines and bind a TCP port on
// every single build.
//
// # Why there are two HTTP surfaces
//
// The page and the JSON API are served by Wails' asset server from the one
// http.Handler this app already had. The MJPEG preview cannot be: WKWebView
// only parses multipart/x-mixed-replace when CFNetwork splits the parts, and
// CFNetwork is not in the path for a custom scheme handler. Measured, the
// frames are accepted and nothing ever renders — a silent black picture with
// no error event, because the stream never ends. So /preview is served from a
// loopback listener instead, and the page points its <img> at that.
//
// The page itself stays on the asset server: Wails' origin validator only
// accepts IPC from the start URL's origin, so a page loaded over loopback
// would have every bound method silently dropped, including the folder picker.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const windowTitle = "Live Record"

func main() {
	log.SetFlags(log.Ltime)

	cfgPath := flag.String("config", defaultConfigPath(), "path to config.json")
	verbose := flag.Bool("v", false, "log the full GStreamer pipeline for each feed")
	flag.Parse()

	app := NewDeferredApp(*cfgPath, *verbose)

	err := wails.Run(&options.App{
		Title:             windowTitle,
		// Two 16:9 feeds sit side by side, so the window wants to be wide rather
		// than tall — the default was portrait-ish and wasted vertical space.
		Width:     1480,
		Height:    760,
		MinWidth:  980,
		MinHeight: 560,
		BackgroundColour:  &options.RGBA{R: 13, G: 15, B: 18, A: 1},
		AssetServer:       &assetserver.Options{Handler: app.Handler()},
		OnStartup:         app.wailsStartup,
		OnShutdown:        app.wailsShutdown,
		Bind:              []any{app},
		// Without an application menu macOS gives the app no Cmd-Q, and no
		// Cmd-C/V/A in any text field.
		Menu: menu.NewMenuFromItems(menu.AppMenu(), menu.EditMenu()),
		Mac: &mac.Options{
			About: &mac.AboutInfo{
				Title:   windowTitle,
				Message: "SRT to growing Apple ProRes.",
			},
		},
		SingleInstanceLock: &options.SingleInstanceLock{UniqueId: "tv.chilton.liverecord"},
	})
	if err != nil {
		log.Fatalf("wails: %v", err)
	}
}

func (a *App) wailsStartup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()

	if err := a.Init(ctx); err != nil {
		log.Printf("startup: %v", err)
		// Surface it rather than dying silently behind a blank window.
		wailsRuntime.MessageDialog(ctx, wailsRuntime.MessageDialogOptions{
			Type:    wailsRuntime.ErrorDialog,
			Title:   "Live Record cannot start",
			Message: err.Error(),
		})
		wailsRuntime.Quit(ctx)
		return
	}

	// The loopback listener carrying /preview. Port 0 when the configured one
	// is taken, so a second instance or a stale process cannot stop the app.
	cfg := a.Config()
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(cfg.ListenPort))
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		log.Printf("preview listener: %v", err)
	} else {
		a.mu.Lock()
		a.previewBase = "http://" + ln.Addr().String()
		a.mu.Unlock()
		go func() { _ = http.Serve(ln, a.Handler()) }()
		log.Printf("preview + remote UI on %s", a.previewBase)
	}

	// In-process pipelines and a native surface per feed: together these are
	// what put the picture inside this window instead of beside it.
	a.mu.Lock()
	a.inProcess = inProcessAvailable
	a.overlay = newPreviewOverlay()
	a.mu.Unlock()

	a.StartFeeds()
	log.Printf("Live Record ready — output %s, ProRes %s, preview %s",
		cfg.OutputDir, cfg.ProResVariant, cfg.PreviewMode)
}

func (a *App) wailsShutdown(ctx context.Context) {
	log.Printf("shutting down, finalising recordings…")
	a.StopFeeds()
	a.mu.RLock()
	ov := a.overlay
	a.mu.RUnlock()
	if ov != nil {
		ov.Close()
	}
	log.Printf("done")
}

// ---------------------------------------------------------------------------
// bound methods, callable from the page

// ChooseOutputFolder opens the native folder picker and returns the chosen
// path, or "" if the operator cancelled.
func (a *App) ChooseOutputFolder() (string, error) {
	a.mu.RLock()
	ctx, cur := a.ctx, ""
	if a.cfg != nil {
		cur = a.cfg.OutputDir
	}
	a.mu.RUnlock()
	if ctx == nil {
		return "", fmt.Errorf("not ready")
	}
	return wailsRuntime.OpenDirectoryDialog(ctx, wailsRuntime.OpenDialogOptions{
		Title:                "Choose where recordings are written",
		DefaultDirectory:     cur,
		CanCreateDirectories: true,
	})
}

// SetPreviewRect tells the native video surface where to sit, in CSS pixels
// with a top-left origin — the same coordinates getBoundingClientRect reports.
// AppKit points and CSS pixels are the same unit here, and the web view fills
// the content view, so no conversion is needed beyond flipping the origin,
// which the overlay does.
//
// The page calls this on every layout and resize. The overlay coalesces
// unchanged rectangles, so a burst costs nothing.
func (a *App) SetPreviewRect(feed string, x, y, w, h int) {
	a.mu.RLock()
	ov := a.overlay
	a.mu.RUnlock()
	if ov == nil || w <= 0 || h <= 0 {
		return
	}
	ov.SetRect(feed, x, y, w, h)
}

// SetPreviewVisible hides a feed's picture without tearing anything down, for
// when the feed stops or the settings sheet covers it.
func (a *App) SetPreviewVisible(feed string, visible bool) {
	a.mu.RLock()
	ov := a.overlay
	a.mu.RUnlock()
	if ov != nil {
		ov.SetVisible(feed, visible)
	}
}

// ExportPanelInstaller writes the portable panel installer somewhere the
// operator chooses, so it can be carried to an edit machine that will never run
// this app. A native save dialog rather than a browser download: WKWebView does
// not handle downloads from the asset server.
func (a *App) ExportPanelInstaller() (string, error) {
	a.mu.RLock()
	ctx := a.ctx
	a.mu.RUnlock()
	if ctx == nil {
		return "", fmt.Errorf("not ready")
	}
	dest, err := wailsRuntime.SaveFileDialog(ctx, wailsRuntime.SaveDialogOptions{
		Title:           "Save the Premiere panel installer",
		DefaultFilename: "LiveRecordRefresh-panel.zip",
	})
	if err != nil || dest == "" {
		return "", err // empty means the operator cancelled
	}
	zipBytes, err := BuildPanelInstaller()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, zipBytes, 0o644); err != nil {
		return "", err
	}
	// Reveal it, so it is obvious where the file went.
	_ = exec.Command("open", "-R", dest).Run()
	return dest, nil
}

// RevealOutputFolder opens the output folder in Finder.
func (a *App) RevealOutputFolder() error {
	cfg := a.Config()
	if cfg == nil {
		return fmt.Errorf("not ready")
	}
	return openInFinder(cfg.OutputDir)
}

func defaultConfigPath() string { return settingsPath() }
