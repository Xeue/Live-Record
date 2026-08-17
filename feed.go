package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type State string

const (
	StateStopped    State = "stopped"
	StateConnecting State = "connecting"
	StateLive       State = "live"      // connected, previewing, not writing
	StateRecording  State = "recording" // connected, previewing, writing ProRes
)

// noMediaTimeout is how long a pipeline may sit up without producing media
// before we call it out. The usual cause is a muxer waiting forever on a pad
// that will never get data (e.g. a video-only feed on an A/V pipeline).
const noMediaTimeout = 15 * time.Second

// Feed supervises one gst-launch process: it dials an SRT listener, decodes,
// optionally records ProRes, and always produces an MJPEG preview which it
// fans out to any number of browser clients.
type Feed struct {
	cfg FeedConfig
	app *App
	// appCfg is the app configuration as it stood when this feed was built.
	// Feeds are recreated wholesale on Reconfigure, so a snapshot is always
	// current for the feed's lifetime and cannot be swapped mid-recording.
	appCfg *Config
	index  int

	mu       sync.Mutex
	want     State
	state    State
	proc     *os.Process
	procDone chan struct{} // closed once the run has fully finished
	// stopFn shuts the current run down gracefully and blocks until the file is
	// finalised. Set per run, because the two engines stop differently: one
	// signals a child, the other pushes EOS through a pipeline it owns.
	stopFn func()
	wake     chan struct{}
	stopping bool // true while we are deliberately taking the process down

	sessionAt time.Time // when Record was pressed; names every part of this take
	part      int
	file     string
	started  time.Time
	duration float64
	size     int64
	restarts int
	lastErr  string
	audioDB  [2]float64
	lastData   time.Time // any sign of life: preview frame, audio level, heartbeat
	lastGrowth time.Time // the recording's duration actually advanced
	stream     string    // what the probe found, e.g. "video=video/x-h264 audio=audio/mpeg"

	frameMu   sync.Mutex
	subs      map[chan []byte]struct{}
	lastFrame []byte
}

func newFeed(app *App, idx int, cfg FeedConfig, appCfg *Config) *Feed {
	return &Feed{
		cfg:     cfg,
		app:     app,
		appCfg:  appCfg,
		index:   idx,
		want:    StateStopped,
		state:   StateStopped,
		wake:    make(chan struct{}, 1),
		audioDB: [2]float64{-96, -96},
		subs:    make(map[chan []byte]struct{}),
	}
}

// SetWant asks the supervisor to move the feed to a new desired state.
func (f *Feed) SetWant(s State) {
	f.mu.Lock()
	if s == StateRecording && f.want != StateRecording {
		// A fresh Record press starts a new session; reconnects inside a
		// session continue with _pt2, _pt3, ... instead.
		f.sessionAt = time.Now()
		f.part = 0
	}
	f.want = s
	if s == StateStopped {
		f.state = StateStopped
	}
	f.stopping = true
	stop := f.stopFn
	f.mu.Unlock()

	// Any change of intent means the current pipeline is wrong: stop it and let
	// the supervisor build the right one.
	if stop != nil {
		stop()
	}

	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *Feed) supervise(ctx context.Context) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		f.mu.Lock()
		want := f.want
		f.mu.Unlock()

		if want == StateStopped {
			select {
			case <-f.wake:
			case <-ctx.Done():
				return
			}
			backoff = time.Second
			continue
		}

		uptime := f.runOnce(ctx, want)

		f.mu.Lock()
		stillWanted := f.want != StateStopped
		f.mu.Unlock()
		if !stillWanted || ctx.Err() != nil {
			backoff = time.Second
			continue
		}

		// The pipeline exited but we still want it up: the SRT peer went away or
		// the stream format changed under us. Back off and redial.
		f.mu.Lock()
		f.restarts++
		f.state = StateConnecting
		f.mu.Unlock()

		if uptime > 30*time.Second {
			backoff = time.Second // it was healthy for a while, retry promptly
		}
		select {
		case <-time.After(backoff):
		case <-f.wake:
		case <-ctx.Done():
			return
		}
		if backoff < 8*time.Second {
			backoff *= 2
		}
	}
}

// stillWants reports whether the intent this pipeline was built for is still
// the intent, and we are not shutting down.
func (f *Feed) stillWants(want State, ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.want == want
}

// runOnce launches a single gst-launch process and blocks until it exits.
// It returns how long the process stayed up.
func (f *Feed) runOnce(ctx context.Context, want State) time.Duration {
	if !f.stillWants(want, ctx) {
		return 0
	}

	// Ask the stream what it carries before building anything. This replaces
	// decodebin's auto-plugging, which on a live feed joined mid-GOP would
	// sometimes fail to link and leave a pipeline that was up but mute.
	f.mu.Lock()
	f.state = StateConnecting
	f.mu.Unlock()

	si, err := f.probe(ctx)
	if err != nil {
		f.setError(err.Error())
		return 0
	}
	if !si.HasVideo() {
		f.setError("feed carries no video — nothing to record")
		return 0
	}
	// The probe takes seconds, during which Stop may have been pressed. There
	// is no process to signal while probing, so without this check we would go
	// on to start a recording that was already cancelled.
	if !f.stillWants(want, ctx) {
		return 0
	}
	f.mu.Lock()
	f.stream = si.String()
	f.mu.Unlock()
	log.Printf("[%s] probed: %s", f.cfg.Name, si)

	var outFile, mxfFile string
	if want == StateRecording && f.appCfg.RecordProRes {
		f.mu.Lock()
		f.part++
		part, at := f.part, f.sessionAt
		f.mu.Unlock()

		base := f.appCfg.resolveFilename(f.cfg.Name, at)
		name := base + ".mov"
		if part > 1 {
			// A QuickTime file cannot be resumed once its writer is gone, so a
			// reconnect continues the take in a new part rather than pretending
			// it can append.
			name = fmt.Sprintf("%s_pt%d.mov", base, part)
		}
		if err := os.MkdirAll(f.appCfg.OutputDir, 0o755); err != nil {
			f.setError("cannot create output dir: " + err.Error())
			return 0
		}
		// filesink opens with O_TRUNC, so an existing name is a destroyed take.
		// Reachable with a pattern like {name}_{date}, or simply by pressing
		// Record, Stop and Record again inside the same second.
		outFile = uniquePath(filepath.Join(f.appCfg.OutputDir, name))
	}
	if want == StateRecording && f.appCfg.RecordAVCIntra {
		f.mu.Lock()
		part, at := f.part, f.sessionAt
		f.mu.Unlock()
		base := f.appCfg.resolveFilename(f.cfg.Name, at)
		if part > 1 {
			base = fmt.Sprintf("%s_pt%d", base, part)
		}
		if err := os.MkdirAll(f.appCfg.OutputDir, 0o755); err != nil {
			f.setError("cannot create output dir: " + err.Error())
			return 0
		}
		mxfFile = uniquePath(filepath.Join(f.appCfg.OutputDir, base+".mxf"))
	}


	// The in-process engine exists so the picture can be composited into the app
	// window: gst_video_overlay_set_window_handle needs the sink in our address
	// space. The command-line build keeps the subprocess engine, where one
	// feed's crash cannot touch another's recording.
	if f.app.useInProcess() {
		return f.runInProcess(ctx, want, si, outFile, mxfFile)
	}
	return f.runSubprocess(ctx, want, si, outFile, mxfFile)
}

// runSubprocess runs the pipeline as a gst-launch child process.
func (f *Feed) runSubprocess(ctx context.Context, want State, si StreamInfo, outFile, mxfFile string) time.Duration {
	// fd 3 in the child: the preview MJPEG stream.
	prevR, prevW, err := os.Pipe()
	if err != nil {
		f.setError("pipe: " + err.Error())
		return 0
	}

	pipeline := f.buildPipeline(want, outFile, mxfFile, si, 3)
	if f.app.verbose {
		log.Printf("[%s] gst-launch-1.0 %s", f.cfg.Name, strings.Join(pipeline, " "))
	}
	cmd := exec.Command(f.app.gstLaunch, pipeline...)
	cmd.ExtraFiles = []*os.File{prevW}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		prevR.Close()
		prevW.Close()
		f.setError(err.Error())
		return 0
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		prevR.Close()
		prevW.Close()
		f.setError(err.Error())
		return 0
	}

	if err := cmd.Start(); err != nil {
		prevR.Close()
		prevW.Close()
		f.setError("gst-launch failed to start: " + err.Error())
		return 0
	}
	// The child owns its copy now; if we held ours open the reader would never
	// see EOF when the pipeline exits.
	prevW.Close()

	procDone := make(chan struct{})

	// Publishing f.proc and re-checking the intent under the SAME lock closes
	// the last gap: a Stop landing between Start and here would otherwise find
	// f.proc still nil, signal nothing, and leave this pipeline running.
	f.mu.Lock()
	if f.want != want || ctx.Err() != nil {
		f.mu.Unlock()
		_ = cmd.Process.Signal(syscall.SIGINT)
		go func() { _ = cmd.Wait() }() // reap without blocking the supervisor
		prevR.Close()
		return 0
	}
	f.proc = cmd.Process
	f.procDone = procDone
	f.stopFn = func() { stopProcess(cmd.Process, procDone) }
	f.stopping = false
	f.state = StateConnecting
	f.file = outFile
	f.started = time.Now()
	f.duration = 0
	f.size = 0
	f.lastErr = ""
	// Start both clocks now. Zero would mean "infinitely stale" and trip the
	// watchdog on the first tick.
	f.lastData = time.Now()
	f.lastGrowth = time.Now()
	f.mu.Unlock()

	// Shutdown must reach the child. supervise only tests ctx between
	// iterations, so without this a cancel during a live recording would wait
	// for a process that has no reason to ever exit.
	ctxStopDone := make(chan struct{})
	defer close(ctxStopDone)
	go func() {
		select {
		case <-ctxStopDone:
		case <-ctx.Done():
			stopProcess(cmd.Process, procDone)
		}
	}()

	log.Printf("[%s] pipeline up (%s) pid=%d %s", f.cfg.Name, want, cmd.Process.Pid, filepath.Base(outFile))

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); f.readMessages(stdout, want) }()
	go func() { defer wg.Done(); f.readErrors(stderr) }()
	go func() { defer wg.Done(); defer prevR.Close(); f.readPreview(prevR) }()

	pollDone := make(chan struct{})
	go func() { defer wg.Done(); f.pollProgress(pollDone, want) }()

	started := time.Now()
	waitErr := cmd.Wait()
	close(procDone) // release anyone blocked in stopProcess
	close(pollDone)
	prevR.Close() // unblock readPreview if the child left the fd open
	wg.Wait()
	uptime := time.Since(started)

	f.finishRun(outFile, mxfFile, uptime, waitErr)
	return uptime
}

// finishRun is the teardown both engines share: discard an empty recording,
// repair the timecode the muxer just re-broke, and settle the feed's state.
func (f *Feed) finishRun(outFile, mxfFile string, uptime time.Duration, runErr error) {
	// An MXF that never received a frame is a stub the muxer opened and nothing
	// wrote. Discard it for the same reason as an empty .mov: a feed retrying a
	// few times at startup would otherwise leave a trail of files that look like
	// takes.
	if mxfFile != "" {
		if st, err := os.Stat(mxfFile); err == nil && st.Size() < 512*1024 {
			if os.Remove(mxfFile) == nil {
				log.Printf("[%s] discarded empty %s", f.cfg.Name, filepath.Base(mxfFile))
			}
		}
	}
	if outFile != "" {
		// A pipeline that never produced a frame still leaves the reserved
		// index on disk — tens of megabytes of empty movie. A feed stuck in a
		// reconnect loop would write one per attempt and fill the volume the
		// good recordings are going to.
		if dur, err := movDuration(outFile); err != nil || dur <= 0 {
			if err := os.Remove(outFile); err == nil {
				log.Printf("[%s] discarded empty recording %s", f.cfg.Name, filepath.Base(outFile))
			}
			f.mu.Lock()
			f.part-- // do not burn a part number on a file that never existed
			f.file = ""
			f.mu.Unlock()
		} else if fixed, err := repairTimecodeOffset(outFile); err != nil {
			log.Printf("[%s] could not repair timecode on finalise: %v", f.cfg.Name, err)
		} else if fixed {
			// Done here and only here: the muxer has stopped, so this is the
			// one moment nothing else is writing the header.
			log.Printf("[%s] repaired time-of-day timecode on finalise", f.cfg.Name)
		}
	}

	f.clearFrames()

	f.mu.Lock()
	deliberate := f.stopping || f.want == StateStopped
	f.proc = nil
	f.procDone = nil
	f.stopFn = nil
	if deliberate {
		f.state = StateStopped
	} else if f.lastErr == "" {
		f.lastErr = "pipeline exited unexpectedly"
		if runErr != nil {
			f.lastErr = "pipeline exited: " + runErr.Error()
		}
	}
	f.mu.Unlock()

	log.Printf("[%s] pipeline down after %s (deliberate=%v)", f.cfg.Name, uptime.Round(time.Second), deliberate)
}

// stopProcess asks gst-launch to shut down cleanly. gst-launch is started with
// -e, so SIGINT makes it push EOS through the pipeline and finalise the moov
// before exiting. We only escalate to SIGKILL if it ignores that.
//
// done is closed by runOnce after its cmd.Wait returns; we must not call Wait
// on the process here or the two would race for the exit status.
func stopProcess(p *os.Process, done <-chan struct{}) {
	if p == nil {
		return
	}
	_ = p.Signal(syscall.SIGINT)
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		_ = p.Kill()
		<-done
	}
}

// ---------------------------------------------------------------------------
// preview fan-out

// subscribe returns a channel of JPEG frames plus a function to release it.
func (f *Feed) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 1)
	f.frameMu.Lock()
	f.subs[ch] = struct{}{}
	last := f.lastFrame
	f.frameMu.Unlock()

	// Hand the newcomer the most recent frame so the image appears immediately
	// rather than after the next encode.
	if last != nil {
		select {
		case ch <- last:
		default:
		}
	}
	return ch, func() {
		f.frameMu.Lock()
		if _, ok := f.subs[ch]; ok {
			delete(f.subs, ch)
			close(ch)
		}
		f.frameMu.Unlock()
	}
}

func (f *Feed) publishFrame(b []byte) {
	f.frameMu.Lock()
	f.lastFrame = b
	for ch := range f.subs {
		// Never block on a slow viewer: drop this frame for them instead.
		select {
		case ch <- b:
		default:
		}
	}
	f.frameMu.Unlock()
}

func (f *Feed) clearFrames() {
	f.frameMu.Lock()
	f.lastFrame = nil
	f.frameMu.Unlock()
}

// readPreview parses the multipart JPEG stream GStreamer writes to fd 3.
// Each part looks like:
//
//	--frame\r\nContent-Type: image/jpeg\r\nContent-Length: N\r\n\r\n<N bytes>\r\n
func (f *Feed) readPreview(r io.Reader) {
	br := bufio.NewReaderSize(r, 512*1024)
	for {
		var length int
		sawHeader := false
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				if sawHeader {
					break // blank line ends this part's headers
				}
				continue // trailing CRLF from the previous part
			}
			sawHeader = true
			if v, ok := strings.CutPrefix(strings.ToLower(line), "content-length:"); ok {
				length, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
		if length <= 0 || length > 32<<20 {
			return // desynchronised or absurd; let the pipeline restart
		}
		buf := make([]byte, length)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		f.publishFrame(buf)

		f.mu.Lock()
		f.lastData = time.Now()
		if f.state == StateConnecting {
			f.state = f.want
		}
		f.mu.Unlock()
	}
}

// ---------------------------------------------------------------------------
// process output

var (
	// GStreamer 1.26 serialises level's rms array as "(GValueArray)< a, b >";
	// older builds use "(double){ a, b }". Accept either.
	reRMS   = regexp.MustCompile(`rms=\((?:GValueArray|double)\)\s*[<{]([^>}]*)[>}]`)
	reError = regexp.MustCompile(`(?i)^ERROR|\(error\): GstMessageError`)

	// The once-a-second progressreport message is the only proof of life for a
	// feed with no audio being previewed natively — there are no level messages
	// and no preview frames to observe. Without consuming it the watchdog
	// mistakes a perfectly healthy pipeline for a dead one.
	reHeartbeat = regexp.MustCompile(`from element "progressreport|\(progress\)`)
)

// readMessages consumes gst-launch's stdout, which carries both the bus
// messages (-m) and, importantly, the human-readable ERROR lines: gst-launch
// prints those to stdout, not stderr, so this is the only place a failed
// element is visible.
func (f *Feed) readMessages(r io.Reader, want State) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()

		if reError.MatchString(line) {
			f.setError(cleanGstMessage(line))
			continue
		}
		if reHeartbeat.MatchString(line) {
			f.mu.Lock()
			f.lastData = time.Now()
			if f.state == StateConnecting {
				f.state = want
			}
			f.mu.Unlock()
			continue
		}
		if m := reRMS.FindStringSubmatch(line); m != nil {
			parts := strings.Split(m[1], ",")
			db := [2]float64{-96, -96}
			for i := 0; i < len(parts) && i < 2; i++ {
				if v, err := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64); err == nil {
					db[i] = v
				}
			}
			if len(parts) == 1 {
				db[1] = db[0] // mono source, mirror to both meters
			}
			f.mu.Lock()
			f.audioDB = db
			f.lastData = time.Now()
			if f.state == StateConnecting {
				f.state = want
			}
			f.mu.Unlock()
		}
	}
}

// readErrors watches stderr, which carries GStreamer's own logging.
func (f *Feed) readErrors(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), "error") {
			f.setError(cleanGstMessage(line))
		}
	}
}

// cleanGstMessage turns GStreamer's escaped debug blobs into something that
// reads sensibly in the UI.
func cleanGstMessage(s string) string {
	if i := strings.Index(s, `debug=(string)"`); i >= 0 {
		s = s[i+len(`debug=(string)"`):]
		s = strings.TrimSuffix(s, `"`)
	}
	s = strings.NewReplacer(`\ `, " ", `\012`, " ", `\"`, `"`, `\\`, `\`, `\(`, "(", `\)`, ")", `\'`, "'").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return trimTo(s, 300)
}

// pollProgress watches the growing file so the UI shows the same duration
// Premiere would see if it re-read the file right now, and flags a pipeline
// that is up but producing nothing.
func (f *Feed) pollProgress(done <-chan struct{}, want State) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			f.mu.Lock()
			path := f.file
			f.mu.Unlock()

			if want == StateRecording && path != "" {
				var size int64
				if st, err := os.Stat(path); err == nil {
					size = st.Size()
				}
				// The timecode repair deliberately does NOT run here.
				//
				// It writes into the moov — and so does the muxer, once a
				// second. Two writers in one header while an NLE is reading it
				// is a documented way to make Premiere misread a growing clip:
				// its index table stops agreeing with the file and the clip
				// comes up with danger stripes. Racing the muxer to correct a
				// display field is not worth risking the recording being
				// editable at all, which is the whole point of the file.
				//
				// The repair runs once, at finalise, in finishRun. The cost is
				// that the timecode reads 00:00:00:00 while the file is still
				// growing, and is correct the moment recording stops.

				dur, err := movDuration(path)
				f.mu.Lock()
				f.size = size
				if err == nil && dur > 0 {
					// Only an *advancing* duration counts as fresh data. Reading
					// the same duration again means the muxer has stopped
					// writing, which is exactly the case "stale" exists to catch.
					if dur > f.duration+0.001 {
						f.lastData = time.Now()
						f.lastGrowth = time.Now()
					}
					f.duration = dur
					if f.state == StateConnecting {
						f.state = StateRecording
					}
				}
				f.mu.Unlock()
			}

			// A pipeline that stops producing media looks identical to a healthy
			// one from the outside: the process is up, no error is printed, and
			// the file sits there at whatever size it reached. Rather than
			// report it and sit there, tear it down — the supervisor re-probes
			// and rebuilds.
			//
			// Judged on two clocks. lastData is any sign of life at all and
			// catches a feed that goes away. lastGrowth is only stamped by the
			// file's duration actually advancing, which is the one that matters
			// while recording: preview frames can keep flowing from a tee while
			// the muxer is wedged and nothing reaches disk.
			growthTimeout := noMediaTimeout
			if d := 3 * time.Duration(f.appCfg.MoovUpdateSec) * time.Second; d > growthTimeout {
				growthTimeout = d // a slow index update is not a stall
			}

			f.mu.Lock()
			var reason string
			switch {
			case time.Since(f.lastData) > noMediaTimeout:
				reason = "no media for " + noMediaTimeout.String()
			case want == StateRecording && f.file != "" &&
				time.Since(f.lastGrowth) > growthTimeout:
				reason = "file stopped growing for " + growthTimeout.String()
			}
			stop := f.stopFn
			if reason != "" {
				f.lastErr = reason + " — reconnecting"
			}
			f.mu.Unlock()

			if reason != "" && stop != nil {
				log.Printf("[%s] %s, restarting pipeline", f.cfg.Name, reason)
				// Through the engine's own stop hook: a pipeline wedged in
				// preroll never acts on EOS, and each engine knows how to
				// escalate past that.
				stop()
				return
			}
		}
	}
}

func (f *Feed) setError(msg string) {
	f.mu.Lock()
	f.lastErr = msg
	f.mu.Unlock()
	log.Printf("[%s] %s", f.cfg.Name, msg)
}

// uniquePath returns path, or path with a numeric suffix if it already exists.
// Never overwrite: an existing file is somebody's take, and the muxer would
// truncate it the instant the pipeline starts.
func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 2; i < 10000; i++ {
		candidate := fmt.Sprintf("%s_%d%s", stem, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	// Absurdly unlikely; a nanosecond suffix is still better than truncating.
	return fmt.Sprintf("%s_%d%s", stem, time.Now().UnixNano(), ext)
}

func trimTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
