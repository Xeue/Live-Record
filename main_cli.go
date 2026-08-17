//go:build !dev && !production && !bindings

// main_cli.go is the plain command-line entry point: a local HTTP server and
// the system browser. It is excluded under the build tags the Wails CLI sets,
// where main_wails.go supplies main() instead.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.Ltime)

	defaultCfg := filepath.Join(mustCwd(), "config.json")
	cfgPath := flag.String("config", defaultCfg, "path to config.json")
	portOverride := flag.Int("port", 0, "override the web UI port")
	outOverride := flag.String("out", "", "override the output directory")
	noOpen := flag.Bool("no-open", false, "do not open the browser on start")
	verbose := flag.Bool("v", false, "log the full GStreamer pipeline for each feed")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app, err := NewApp(ctx, *cfgPath, *verbose)
	if err != nil {
		log.Fatal(err)
	}

	// Command-line overrides win over the file, but are not written back to it.
	if *portOverride != 0 || *outOverride != "" {
		cfg := app.Config()
		if *portOverride != 0 {
			cfg.ListenPort = *portOverride
		}
		if *outOverride != "" {
			cfg.OutputDir = *outOverride
		}
		if err := cfg.normalise(); err != nil {
			log.Fatalf("config: %v", err)
		}
		if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
			log.Fatalf("output folder: %v", err)
		}
		app.mu.Lock()
		app.cfg = cfg
		app.mu.Unlock()
	}

	app.StartFeeds()

	cfg := app.Config()
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.ListenPort)
	srv := &http.Server{Addr: addr, Handler: app.Handler()}

	log.Printf("Live Record ready on http://%s", addr)
	log.Printf("  output   %s", cfg.OutputDir)
	log.Printf("  codec    Apple ProRes %s + PCM 24-bit, growing QuickTime", strings.ToUpper(cfg.ProResVariant))
	log.Printf("  preview  %s", cfg.PreviewMode)
	for _, fc := range cfg.Feeds {
		log.Printf("  feed     %-10s %s", fc.Name, fc.URL)
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	if !*noOpen {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = exec.Command("open", "http://"+addr).Run()
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Printf("shutting down, finalising recordings…")
	app.StopFeeds()
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	log.Printf("done")
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}
