//go:build darwin && cgo

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The in-process engine has to reproduce what gst-launch's -e flag did: push
// EOS, wait for it to come back up the bus, and only then go to NULL. If that
// ordering is wrong the recording is left unfinalised, so this records a real
// ProRes file and checks the result rather than trusting the sequence.
func TestGstPipelineRecordsAndFinalises(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cgo.mov")

	desc := fmt.Sprintf(`videotestsrc pattern=ball is-live=true `+
		`! video/x-raw,width=320,height=240,framerate=25/1 `+
		`! timecodestamper source=rtc `+
		`! vtenc_prores ! video/x-prores,variant=(string)lt ! queue ! mux.video_0 `+
		`audiotestsrc is-live=true wave=sine `+
		`! audioconvert ! audioresample ! level interval=200000000 `+
		`! audioconvert ! audio/x-raw,rate=48000,channels=2,format=S24BE ! queue ! mux.audio_0 `+
		`qtmux name=mux reserved-max-duration=3600000000000 `+
		`reserved-moov-update-period=1000000000 force-create-timecode-trak=true `+
		`! filesink name=sink location=%q sync=false`, "PLACEHOLDER")

	p, err := gstParse(desc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer p.Close()

	// Paths go through g_object_set, not the description parser, because the
	// parser has its own escaping rules and a real output folder has spaces.
	p.SetString("sink", "location", out)

	if err := p.Play(); err != nil {
		t.Fatalf("play: %v", err)
	}

	// Collect proof that the bus is readable without regex scraping.
	var levels, errs, withAudio int
	loudest := -200.0
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		msg, ok := p.Poll(200 * time.Millisecond)
		if !ok {
			continue
		}
		switch msg.Kind {
		case gstMsgLevel:
			levels++
			if msg.RMS[0] > loudest {
				loudest = msg.RMS[0]
			}
			if msg.RMS[0] > -60 {
				withAudio++
			}
		case gstMsgError:
			errs++
			t.Logf("bus error: %s", msg.Text)
		}
	}
	if errs > 0 {
		t.Fatalf("%d errors on the bus", errs)
	}
	if levels == 0 {
		t.Fatal("no level messages read from the bus — audio metering would be dead")
	}
	// Counting messages is not enough: level posts its RMS as a GLib
	// GValueArray, not GStreamer's own array type, so reading the wrong one
	// yields messages with no values and meters pinned at the floor.
	if withAudio == 0 {
		t.Errorf("read %d level messages but no usable RMS values (loudest %.1f dB) — "+
			"the array type is being misread", levels, loudest)
	}

	if err := p.Finalise(10 * time.Second); err != nil {
		t.Fatalf("finalise: %v", err)
	}

	st, err := os.Stat(out)
	if err != nil {
		t.Fatalf("no output file: %v", err)
	}
	if st.Size() < 1024 {
		t.Fatalf("output is %d bytes", st.Size())
	}

	// The real test: the file must be a readable QuickTime movie with a
	// sensible duration, which is only true if the index was finalised.
	dur, err := movDuration(out)
	if err != nil {
		t.Fatalf("output is not a readable QuickTime file: %v", err)
	}
	if dur < 1.0 {
		t.Errorf("duration %.2fs — the index was not finalised", dur)
	}
	t.Logf("recorded %.2fs, %d level messages, %d bytes", dur, levels, st.Size())
}

// A bad description must come back as an error rather than a crash or a nil
// pipeline that fails later.
func TestGstParseRejectsBadPipeline(t *testing.T) {
	if p, err := gstParse("thiselementdoesnotexist ! fakesink"); err == nil {
		p.Close()
		t.Error("expected an error for an unknown element")
	}
}

// Close must be safe to call twice: teardown paths overlap.
func TestGstPipelineCloseIsIdempotent(t *testing.T) {
	p, err := gstParse("fakesrc num-buffers=1 ! fakesink")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p.Close()
	p.Close()
}
