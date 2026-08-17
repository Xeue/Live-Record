package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// App owns the configuration and the set of running feeds.
//
// Feeds are replaced wholesale when the configuration changes rather than
// mutated in place: a feed's identity is its pipeline, and there is no safe way
// to change an SRT endpoint or an output path underneath a running recording.
type App struct {
	cfgPath   string
	gstLaunch string
	gstIns    string
	verbose   bool

	muxOnce sync.Once
	mux     *http.ServeMux

	mu       sync.RWMutex
	cfg      *Config
	feeds    []*Feed
	feedStop context.CancelFunc
	feedWG   sync.WaitGroup

	baseCtx context.Context
	// ctx is the Wails context, needed for runtime calls such as the folder
	// picker. Nil in the CLI build.
	ctx context.Context

	// inProcess selects the engine. The app sets it so the picture can be
	// composited into its window; the command-line build leaves it false and
	// keeps the crash isolation of one child process per feed.
	inProcess bool
	// overlay owns the native views the video sinks draw into. Nil when there
	// is no window, in which case each sink opens its own.
	overlay previewOverlay

	// previewBase prefixes the MJPEG URL the page uses. Empty in the CLI build,
	// where the page and the preview share an origin. Under Wails it points at
	// a loopback listener, because multipart/x-mixed-replace cannot be
	// delivered through a WKWebView custom scheme handler — the frames arrive
	// and nothing ever renders.
	previewBase string
}

// NewDeferredApp returns an App that has not touched the filesystem or
// GStreamer yet. Wails' bindings build step runs main() without ever calling
// OnStartup, so anything done before wails.Run would spawn pipelines and bind
// ports on every build.
func NewDeferredApp(cfgPath string, verbose bool) *App {
	return &App{cfgPath: cfgPath, verbose: verbose}
}

// Init loads configuration and verifies GStreamer. Safe to call once.
func (a *App) Init(ctx context.Context) error {
	// Before ANY GStreamer call or child process: a bundled .app carries its own
	// plugins and they are found only through the environment, which gst_init
	// reads once.
	setupGStreamerEnv()

	cfg, err := loadConfig(a.cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	gstLaunch, err := findGst("gst-launch-1.0")
	if err != nil {
		return err
	}
	gstIns, err := findGst("gst-inspect-1.0")
	if err != nil {
		return err
	}
	if err := preflight(gstIns); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("output folder: %w", err)
	}

	a.mu.Lock()
	a.cfg, a.gstLaunch, a.gstIns, a.baseCtx = cfg, gstLaunch, gstIns, ctx
	a.mu.Unlock()
	return nil
}

// Elements the pipelines depend on. Checking up front turns a confusing
// mid-record failure into a clear message at startup.
// These are exactly what buildPipeline can emit. Checking a stale list lets a
// partial GStreamer install pass startup and fail at the moment Record is
// pressed, which is the failure preflight exists to prevent.
var requiredElements = []string{
	// source and demux
	"srtsrc", "tsdemux", "queue",
	// parsers and decoders the probe can select
	"h264parse", "h265parse", "mpegvideoparse", "aacparse", "ac3parse", "mpegaudioparse",
	"vtdec_hw", "avdec_mpeg2video", "avdec_aac", "avdec_ac3", "avdec_mp3",
	// video path
	"videoconvert", "timecodestamper", "progressreport", "tee",
	// record path — ProRes/QuickTime
	"vtenc_prores", "qtmux", "filesink",
	// record path — AVC-Intra/MXF. x264enc and mxfmux were added with that
	// output and not added here, which cost a shipped bundle: ship.sh derives
	// what to bundle FROM this list, so a missing entry is not a startup warning
	// but a plugin absent from the .app on a machine with no Homebrew.
	"x264enc", "mxfmux",
	// audio path
	"audioconvert", "audioresample", "level", "fakesink",
	// preview paths
	"glimagesink", "videorate", "videoscale", "jpegenc", "multipartmux", "fdsink",
}

func NewApp(ctx context.Context, cfgPath string, verbose bool) (*App, error) {
	a := NewDeferredApp(cfgPath, verbose)
	if err := a.Init(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// previewOverlay is a set of native views, one per feed, that video sinks draw
// into. Implemented on macOS with cgo; absent everywhere else.
type previewOverlay interface {
	// Handle returns the native view for a feed, creating it if needed.
	// Returns 0 if there is no window to attach to yet.
	Handle(feed string) uintptr
	// SetRect positions a feed's view in window coordinates, top-left origin.
	SetRect(feed string, x, y, w, h int)
	// SetVisible hides a view without destroying it.
	SetVisible(feed string, visible bool)
	Close()
}

// useInProcess reports whether pipelines should run in this process.
func (a *App) useInProcess() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return inProcessAvailable && a.inProcess
}

// PreviewHandle returns the native view a feed's video sink should render into,
// or 0 to let the sink open its own window.
func (a *App) PreviewHandle(feed string) uintptr {
	a.mu.RLock()
	ov := a.overlay
	a.mu.RUnlock()
	if ov == nil {
		return 0
	}
	return ov.Handle(feed)
}

// Config returns a copy so callers cannot mutate live configuration.
func (a *App) Config() *Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.clone()
}

// Feeds returns the current feed set for iteration.
func (a *App) Feeds() []*Feed {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.feeds
}

func (a *App) feedByName(name string) *Feed {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, f := range a.feeds {
		if strings.EqualFold(f.cfg.Name, name) {
			return f
		}
	}
	return nil
}

// StartFeeds builds a feed per configured source and starts its supervisor.
func (a *App) StartFeeds() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.startFeedsLocked()
}

func (a *App) startFeedsLocked() {
	ctx, cancel := context.WithCancel(a.baseCtx)
	a.feedStop = cancel
	a.feeds = nil
	for i, fc := range a.cfg.Feeds {
		f := newFeed(a, i, fc, a.cfg)
		a.feeds = append(a.feeds, f)
		a.feedWG.Add(1)
		go func(f *Feed) { defer a.feedWG.Done(); f.supervise(ctx) }(f)
	}
}

// StopFeeds takes every feed down, finalising any recording in progress.
func (a *App) StopFeeds() {
	a.mu.Lock()
	feeds, cancel := a.feeds, a.feedStop
	a.mu.Unlock()

	// Ask every feed to stop concurrently: each SIGINTs its gst-launch, which
	// pushes EOS and writes the final moov, and that takes a moment per feed.
	var wg sync.WaitGroup
	for _, f := range feeds {
		wg.Add(1)
		go func(f *Feed) { defer wg.Done(); f.SetWant(StateStopped) }(f)
	}
	wg.Wait()

	if cancel != nil {
		cancel()
	}
	a.feedWG.Wait()
}

// Reconfigure validates and applies a new configuration.
//
// Every feed is stopped first — including any that are recording. That is
// deliberate and the UI says so: silently carrying a recording across a change
// of output path or SRT endpoint would be worse than stopping it.
func (a *App) Reconfigure(next *Config) error {
	if err := next.normalise(); err != nil {
		return err
	}
	if err := os.MkdirAll(next.OutputDir, 0o755); err != nil {
		return fmt.Errorf("output folder %q: %w", next.OutputDir, err)
	}
	// Fail before stopping anything if the folder is not writable.
	probe := filepath.Join(next.OutputDir, ".liverecord-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("cannot write to %q: %w", next.OutputDir, err)
	}
	_ = os.Remove(probe)

	a.mu.RLock()
	portChanged := next.ListenPort != a.cfg.ListenPort
	a.mu.RUnlock()

	a.StopFeeds()

	a.mu.Lock()
	a.cfg = next
	a.startFeedsLocked()
	a.mu.Unlock()

	if err := next.save(a.cfgPath); err != nil {
		return fmt.Errorf("saved settings applied, but writing %s failed: %w", a.cfgPath, err)
	}
	log.Printf("configuration updated (%d feeds, %s, ProRes %s)",
		len(next.Feeds), next.OutputDir, strings.ToUpper(next.ProResVariant))
	if portChanged {
		log.Printf("note: the web UI port changes on next launch")
	}
	return nil
}

// findGst locates a GStreamer tool, falling back to the Homebrew prefix since
// app-bundle and launchd processes often have a minimal PATH.
func findGst(name string) (string, error) {
	// A bundled copy wins: it matches the bundled plugins and libraries, whereas
	// whatever Homebrew has may be a different version entirely.
	if res := bundleResources(); res != "" {
		p := filepath.Join(filepath.Dir(res), "MacOS", name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found. Install GStreamer with: brew install gstreamer", name)
}

// checkFolder reports whether a folder can actually take a recording, and how
// much room is left. Finding out at Record time, on a show, is too late.
func checkFolder(path string) (freeGB float64, err error) {
	if strings.TrimSpace(path) == "" {
		return 0, fmt.Errorf("no folder given")
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return 0, fmt.Errorf("cannot create it: %w", err)
	}
	probe := filepath.Join(path, ".liverecord-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return 0, fmt.Errorf("not writable: %w", err)
	}
	_ = os.Remove(probe)
	return diskFreeGB(path), nil
}

func diskFreeGB(path string) float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return float64(st.Bavail) * float64(st.Bsize) / (1 << 30)
}

// preflight checks every element the pipelines can use.
//
// Concurrently: this is one process spawn per element, and run in sequence it
// added a visible delay to startup — long enough that the window opened before
// the app was ready.
func preflight(inspect string) error {
	type result struct {
		name string
		ok   bool
	}
	// Warm the plugin registry ONCE, serially, before fanning out.
	//
	// A bundled app starts with no registry, and every gst-inspect child builds
	// it. Eight of them racing to write the same GST_REGISTRY_1_0 leaves it in a
	// state the first pipelines then have to rebuild — measured on a
	// self-contained bundle as two failed record attempts on one feed before the
	// third succeeded, each leaving an empty file behind.
	_ = exec.Command(inspect, "--exists", requiredElements[0]).Run()

	results := make(chan result, len(requiredElements))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, el := range requiredElements {
		wg.Add(1)
		go func(el string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results <- result{el, exec.Command(inspect, "--exists", el).Run() == nil}
		}(el)
	}
	wg.Wait()
	close(results)

	var missing []string
	for r := range results {
		if !r.ok {
			missing = append(missing, r.name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("GStreamer is missing required elements: %s\n"+
			"Install the full plugin set with: brew install gstreamer",
			strings.Join(missing, ", "))
	}
	return nil
}
