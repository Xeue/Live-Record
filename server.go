package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"
)

//go:embed web/*
var webFS embed.FS

type feedStatus struct {
	Name     string     `json:"name"`
	URL      string     `json:"url"`
	State    string     `json:"state"`
	File     string     `json:"file"`
	Duration float64    `json:"duration"`
	Size     int64      `json:"size"`
	Restarts int        `json:"restarts"`
	Error    string     `json:"error"`
	AudioDB  [2]float64 `json:"audioDb"`
	Stale    bool       `json:"stale"`
	Part     int        `json:"part"`
	Stream   string     `json:"stream"`
	// StartedAt is when the current recording began, RFC3339. The UI shows
	// time of day alongside elapsed, because in sports the operative question
	// is what time something happened.
	StartedAt string `json:"startedAt"`
}

type appStatus struct {
	Feeds      []feedStatus `json:"feeds"`
	OutputDir  string       `json:"outputDir"`
	Variant    string       `json:"proresVariant"`
	PreviewMode string      `json:"previewMode"`
	DiskFreeGB float64      `json:"diskFreeGb"`
	Now        string       `json:"now"` // server clock, so the UI TOD matches the timecode
	// Formats is what each feed is actually writing, for the header readout.
	Formats []string `json:"formats"`
	// PreviewBase prefixes the MJPEG URL. Empty in the CLI build; under Wails
	// it points at the loopback listener, because multipart streams cannot be
	// delivered through the webview's custom scheme handler.
	PreviewBase string `json:"previewBase"`
}

func (f *Feed) status() feedStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := feedStatus{
		Name:     f.cfg.Name,
		URL:      f.cfg.URL,
		State:    string(f.state),
		File:     f.file,
		Duration: f.duration,
		Size:     f.size,
		Restarts: f.restarts,
		Error:    f.lastErr,
		AudioDB:  f.audioDB,
		Part:     f.part,
		Stream:   f.stream,
	}
	// "Stale" means the process is up but media stopped arriving — the case
	// that matters most during a live record and that a plain state flag hides.
	if (f.state == StateLive || f.state == StateRecording) &&
		!f.lastData.IsZero() && time.Since(f.lastData) > 3*time.Second {
		st.Stale = true
	}
	if f.state == StateRecording && !f.sessionAt.IsZero() {
		st.StartedAt = f.sessionAt.Format(time.RFC3339)
	} else {
		st.File = ""
	}
	return st
}

// Handler returns the app's HTTP handler, built once.
//
// The page is always served, even before startup has finished. Returning an
// error for the document instead means the webview renders that error as the
// whole UI and never asks again — the window just says "starting up" for ever.
// Only the API waits for readiness; the page polls it and fills in when it
// starts answering.
func (a *App) Handler() http.Handler {
	a.muxOnce.Do(func() { a.mux = a.routes() })
	return a.mux
}

func (a *App) ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg != nil
}

func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// api guards a handler that needs configuration to exist.
	api := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !a.ready() {
				http.Error(w, "starting up", http.StatusServiceUnavailable)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /api/state", api(func(w http.ResponseWriter, r *http.Request) {
		cfg := a.Config()
		a.mu.RLock()
		previewBase := a.previewBase
		a.mu.RUnlock()
		out := appStatus{
			OutputDir:   cfg.OutputDir,
			Variant:     cfg.ProResVariant,
			PreviewMode: cfg.PreviewMode,
			DiskFreeGB:  diskFreeGB(cfg.OutputDir),
			Now:         time.Now().Format(time.RFC3339),
			PreviewBase: previewBase,
		}
		if cfg.RecordProRes {
			out.Formats = append(out.Formats, "ProRes "+strings.ToUpper(cfg.ProResVariant))
		}
		if cfg.RecordAVCIntra {
			out.Formats = append(out.Formats, fmt.Sprintf("AVC-Intra %d", cfg.AVCIntraClass))
		}
		for _, f := range a.Feeds() {
			out.Feeds = append(out.Feeds, f.status())
		}
		writeJSON(w, out)
	}))

	mux.HandleFunc("POST /api/feeds/{name}/{action}", api(func(w http.ResponseWriter, r *http.Request) {
		f := a.feedByName(r.PathValue("name"))
		if f == nil {
			http.Error(w, "unknown feed", http.StatusNotFound)
			return
		}
		if err := applyAction(f, r.PathValue("action")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, f.status())
	}))

	mux.HandleFunc("POST /api/all/{action}", api(func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		for _, f := range a.Feeds() {
			if err := applyAction(f, action); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("GET /preview/{name}", api(a.handlePreview))

	mux.HandleFunc("GET /api/config", api(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"config":         a.Config(),
			"proresVariants": ProResVariants,
			"previewModes":   PreviewModes,
		})
	}))

	// Saving settings stops every feed, including any that are recording:
	// changing an output path or SRT endpoint under a live recording would be
	// worse than interrupting it. The UI warns before posting here.
	mux.HandleFunc("POST /api/config", api(func(w http.ResponseWriter, r *http.Request) {
		var next Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&next); err != nil {
			http.Error(w, "could not read settings: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.Reconfigure(&next); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"config": a.Config()})
	}))

	// Report whether a folder is usable before the operator commits to it.
	mux.HandleFunc("POST /api/check-folder", api(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		free, err := checkFolder(body.Path)
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "freeGb": free})
	}))

	// The Premiere panel: status, install, remove. Not gated on readiness — it
	// is filesystem work that does not depend on any feed being configured.
	mux.HandleFunc("GET /api/panel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, PanelState())
	})
	mux.HandleFunc("POST /api/panel/install", func(w http.ResponseWriter, r *http.Request) {
		st, err := InstallPanel()
		if err != nil {
			writeJSON(w, map[string]any{"status": st, "error": err.Error()})
			return
		}
		log.Printf("Premiere panel installed to %s", st.Path)
		writeJSON(w, map[string]any{"status": st})
	})
	mux.HandleFunc("POST /api/panel/remove", func(w http.ResponseWriter, r *http.Request) {
		st, err := RemovePanel()
		if err != nil {
			writeJSON(w, map[string]any{"status": st, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"status": st})
	})

	// Download the portable installer, for a machine that will never run this app.
	mux.HandleFunc("GET /api/panel/installer", func(w http.ResponseWriter, r *http.Request) {
		zipBytes, err := BuildPanelInstaller()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="LiveRecordRefresh-panel.zip"`)
		_, _ = w.Write(zipBytes)
	})

	web, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embedded web assets: %v", err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(web)))
	return mux
}

func applyAction(f *Feed, action string) error {
	switch action {
	case "monitor":
		f.SetWant(StateLive)
	case "record":
		f.SetWant(StateRecording)
	case "stop":
		f.SetWant(StateStopped)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return nil
}

// handlePreview streams the feed's JPEG frames to the browser as
// multipart/x-mixed-replace, which an <img> tag renders as live video.
// Frames come from the feed's fan-out, so any number of viewers can watch and
// a slow one only ever drops its own frames.
func (a *App) handlePreview(w http.ResponseWriter, r *http.Request) {
	f := a.feedByName(r.PathValue("name"))
	if f == nil {
		http.Error(w, "unknown feed", http.StatusNotFound)
		return
	}

	// Without the browser branch in the pipeline there are no JPEG frames, and
	// blocking here would leave the client hanging on unflushed headers with no
	// way to tell a misconfiguration from a dead feed.
	if !a.Config().browserPreview() {
		http.Error(w, "browser preview is off for this app (preview mode is "+
			a.Config().PreviewMode+"); set it to \"browser\" or \"both\" in Settings",
			http.StatusConflict)
		return
	}

	frames, release := f.subscribe()
	defer release()

	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	// Get the headers on the wire now, so a client sees a live response even
	// before the first frame arrives.
	if flusher != nil {
		flusher.Flush()
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w,
				"--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(frame)); err != nil {
				return
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
