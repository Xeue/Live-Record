//go:build darwin && cgo

package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// inProcessAvailable reports that this build can run pipelines in-process.
const inProcessAvailable = true

// runInProcess runs the pipeline inside this process rather than as a child.
//
// It exists for one reason: gst_video_overlay_set_window_handle() has to be
// called on the sink element, so the sink must be in our address space for the
// picture to be composited into the app window.
//
// The two things gst-launch was doing for us are replaced here:
//
//   - -e (EOS on interrupt) becomes an explicit EOS event followed by waiting
//     for EOS to come back up the bus before going to NULL. That wait is what
//     writes the final index; skipping it truncates every recording.
//   - -m (print bus messages) becomes reading the bus directly, which turns the
//     audio meters and the heartbeat from scraped text into typed values.
func (f *Feed) runInProcess(ctx context.Context, want State, si StreamInfo, outFile, mxfFile string) time.Duration {
	// A raw pipe for the browser preview. syscall.Pipe rather than os.Pipe: the
	// write end is handed to fdsink by number and must stay a plain blocking
	// descriptor that Go's network poller has never seen.
	//
	// Unlike the subprocess engine, THIS write end is not a copy: fdsink uses the
	// descriptor we give it, in this process, and does not dup it. Closing it
	// while the pipeline runs is closing the sink's own output — measured, the
	// pipeline dies within a second with "Error while writing to file descriptor:
	// Bad file descriptor" and both formats stop. So it is closed at teardown,
	// after the pipeline is at NULL, which is also what gives the reader its EOF.
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		f.setError("preview pipe: " + err.Error())
		return 0
	}
	prevR := os.NewFile(uintptr(fds[0]), "preview-r")
	prevWFD := fds[1]
	closeWrite := func() {
		if prevWFD >= 0 {
			syscall.Close(prevWFD)
			prevWFD = -1
		}
	}

	// The description is the same one the subprocess engine uses, minus the
	// two gst-launch flags, which have no meaning here.
	args := f.buildPipeline(want, outFile, mxfFile, si, fds[1])
	desc := strings.Join(args[2:], " ")
	if f.app.verbose {
		log.Printf("[%s] in-process pipeline: %s", f.cfg.Name, desc)
	}

	pipe, err := gstParse(desc)
	if err != nil {
		prevR.Close()
		closeWrite()
		f.setError("pipeline: " + err.Error())
		return 0
	}

	// Paths and passphrases are set as properties, not through the description.
	// The parser has its own escaping rules and an output folder with a space in
	// it is exactly the shape that goes wrong.
	if outFile != "" {
		pipe.SetString("sink"+f.elementSuffix(), "location", outFile)
	}
	if f.cfg.Passphrase != "" {
		pipe.SetString("src"+f.elementSuffix(), "passphrase", f.cfg.Passphrase)
	}

	// Hand the sink our composited view, if the app has one. Failure here is not
	// fatal: without a handle glimagesink opens its own window, which is worse
	// looking but still a working preview.
	if f.appCfg.nativePreview() {
		if h := f.app.PreviewHandle(f.cfg.Name); h != 0 {
			if err := pipe.SetWindowHandle("preview"+f.elementSuffix(), h); err != nil {
				log.Printf("[%s] compositing unavailable, using a separate window: %v", f.cfg.Name, err)
			}
		}
	}

	runDone := make(chan struct{})
	eos := make(chan struct{})
	var eosOnce sync.Once

	// Publishing the stop hook and re-checking intent under the same lock closes
	// the same race the subprocess engine has: a Stop landing between here and
	// the state change would otherwise find no hook and leave this running.
	f.mu.Lock()
	if f.want != want || ctx.Err() != nil {
		f.mu.Unlock()
		pipe.Close()
		prevR.Close()
		closeWrite()
		return 0
	}
	f.procDone = runDone
	f.stopFn = func() {
		// Push EOS and wait for the single bus reader to see it come back.
		// gst_bus_timed_pop from two goroutines would let one steal the other's
		// EOS, so the wait happens here on a channel rather than by polling.
		pipe.SendEOS()
		select {
		case <-eos:
		case <-time.After(15 * time.Second):
			log.Printf("[%s] EOS did not arrive; the last index update ends the file", f.cfg.Name)
		}
		_ = pipe.Null()
		<-runDone
	}
	f.stopping = false
	f.state = StateConnecting
	f.file = outFile
	f.started = time.Now()
	f.duration = 0
	f.size = 0
	f.lastErr = ""
	f.lastData = time.Now()
	f.lastGrowth = time.Now()
	f.mu.Unlock()

	if err := pipe.Play(); err != nil {
		f.mu.Lock()
		f.stopFn, f.procDone = nil, nil
		f.mu.Unlock()
		close(runDone)
		pipe.Close()
		prevR.Close()
		closeWrite()
		f.setError("could not start pipeline: " + err.Error())
		return 0
	}
	log.Printf("[%s] pipeline up in-process (%s) state=%s %s",
		f.cfg.Name, want, pipe.StateName(), filepath.Base(outFile))

	// The write end stays open for the life of the pipeline: it IS fdsink's
	// output descriptor, not a copy of it. See the note where the pipe is made.

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer prevR.Close(); f.readPreview(prevR) }()
	pollDone := make(chan struct{})
	go func() { defer wg.Done(); f.pollProgress(pollDone, want) }()

	started := time.Now()

	// The single bus reader.
	var runErr error
	for {
		msg, ok := pipe.Poll(250 * time.Millisecond)
		if !ok {
			if ctx.Err() != nil {
				break
			}
			continue
		}
		switch msg.Kind {
		case gstMsgEOS:
			eosOnce.Do(func() { close(eos) })
		case gstMsgError:
			runErr = errors.New(msg.Text)
			f.setError(msg.Text)
		case gstMsgWarning:
			// Warnings are routine on reconnect; note them without painting the
			// feed red for the rest of the take.
			if f.app.verbose {
				log.Printf("[%s] %s", f.cfg.Name, msg.Text)
			}
		case gstMsgLevel:
			f.mu.Lock()
			f.audioDB = msg.RMS
			f.lastData = time.Now()
			if f.state == StateConnecting {
				f.state = want
			}
			f.mu.Unlock()
		case gstMsgLatency:
			// Required of the application: an element's latency changed and the
			// pipeline must redistribute it, or sinks start dropping.
			pipe.RecalculateLatency()
		case gstMsgClockLost:
			// The clock went away (a source reconnecting). Re-select one by
			// cycling through PAUSED, as any live application must.
			_ = pipe.Pause()
			_ = pipe.Play()
		case gstMsgProgress:
			f.mu.Lock()
			f.lastData = time.Now()
			if f.state == StateConnecting {
				f.state = want
			}
			f.mu.Unlock()
		}
		if msg.Kind == gstMsgEOS || msg.Kind == gstMsgError {
			break
		}
	}

	eosOnce.Do(func() { close(eos) }) // release a stopFn waiting on us
	close(pollDone)
	uptime := time.Since(started)

	// Order matters here, in three ways.
	//
	// The pipeline goes to NULL first, so fdsink has stopped writing before its
	// descriptor is closed — closing it underneath a live sink would raise
	// SIGPIPE on a GStreamer thread, which Go forwards to the default handler
	// and which would take the whole app down.
	//
	// runDone is then closed BEFORE wg.Wait(), which the subprocess engine has
	// always done and this one did not. stopFn ends with <-runDone, and the
	// no-media watchdog calls stopFn from pollProgress — a goroutine wg.Wait()
	// is waiting on. Closing runDone afterwards deadlocked every watchdog
	// restart: measured, a stalled feed sat there for the rest of the session
	// and would not even shut down.
	//
	// Only then is the write end closed, which is what gives the preview reader
	// its EOF.
	pipe.Close()
	close(runDone)
	closeWrite()
	prevR.Close()
	wg.Wait()

	f.finishRun(outFile, uptime, runErr)
	return uptime
}
