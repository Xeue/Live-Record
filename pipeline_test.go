package main

import (
	"strings"
	"testing"
)

// bothFormatsPipeline builds the description for the configuration that used to
// deadlock: one feed writing ProRes AND AVC-Intra MXF, with a composited
// preview, i.e. three sinks fed from one tee.
func bothFormatsPipeline(t *testing.T, previewMode string) []string {
	t.Helper()
	cfg := defaultConfig()
	cfg.PreviewMode = previewMode
	cfg.RecordProRes = true
	cfg.RecordAVCIntra = true
	if err := cfg.normalise(); err != nil {
		t.Fatalf("config: %v", err)
	}
	f := &Feed{cfg: FeedConfig{Name: "CAM-A", Host: "127.0.0.1", Port: 9001}, appCfg: cfg}
	si := StreamInfo{
		ProgramNumber: 1,
		VideoCaps:     "video/x-h264", VideoParser: "h264parse", VideoDec: "vtdec_hw",
		AudioCaps: "audio/mpeg", AudioParser: "aacparse", AudioDec: "avdec_aac",
	}
	return f.buildPipeline(StateRecording, "/tmp/x.mov", "/tmp/x.mxf", si, 3)
}

// Every sink must carry async=false.
//
// A GstBaseSink completes READY->PAUSED only once it has been given a buffer,
// and a GstBin holds every child in PAUSED until all of its async children are
// finished. With both formats enabled the MXF sink cannot get a buffer until
// x264enc emits its first frame, and x264enc cannot get frames because the
// ProRes branch's queues have filled behind a filesink that is itself parked in
// gst_base_sink_wait_preroll. The pipeline never leaves PAUSED and every file
// stays at zero bytes.
//
// The condition is a race against how long the slowest sink takes to preroll,
// so a functional test of it is a coin toss. The invariant is not: no sink in a
// live recording pipeline may gate the state change.
func TestEverySinkIsTakenOutOfPreroll(t *testing.T) {
	for _, mode := range PreviewModes {
		t.Run(mode, func(t *testing.T) {
			args := bothFormatsPipeline(t, mode)

			// Element names, not properties: "fdsink"/"filesink" both end in
			// "sink", and so does every real sink GStreamer ships.
			var sinks []int
			for i, a := range args {
				if strings.HasSuffix(a, "sink") && !strings.Contains(a, "=") {
					sinks = append(sinks, i)
				}
			}
			if len(sinks) < 3 {
				t.Fatalf("expected at least 3 sinks (ProRes, MXF, preview), found %d in %v",
					len(sinks), args)
			}
			for _, i := range sinks {
				// A sink's properties run from its name up to the next link or
				// the next element reference.
				found := false
				for j := i + 1; j < len(args) && args[j] != "!" && strings.Contains(args[j], "="); j++ {
					if args[j] == "async=false" {
						found = true
					}
				}
				if !found {
					t.Errorf("sink %q has no async=false — it will gate the pipeline's "+
						"state change and can deadlock it in PAUSED", args[i])
				}
			}
		})
	}
}

// No queue feeding mxfmux may be leaky.
//
// mxfmux writes an index entry per edit unit and asserts that the presentation
// position and the written position stay within a signed byte of each other:
//
//	index_pos_diff = pts_pos - pad->pos;
//	g_assert (index_pos_diff < 127 && index_pos_diff >= -127);
//
// A dropped buffer widens that gap permanently, so the 128th drop aborts the
// whole process — both feeds, mid-take, neither file finalised. Measured: a run
// whose MXFs came out 106 and 196 frames short of their ProRes masters died
// exactly there.
func TestMxfBranchNeverDropsBuffers(t *testing.T) {
	args := bothFormatsPipeline(t, "native")

	// Each branch runs from a tee-pad reference ("v_CAMA0." / "a_CAMA0.") to
	// whatever it ends in. Checking only the queue nearest the muxer is not
	// enough: on the video side that is the one after h264parse, while the leaky
	// one sat at the head of the branch, before videoconvert and x264enc. So
	// every queue on any path into mxfmux is checked.
	isRef := func(s string) bool { return strings.HasSuffix(s, ".") && !strings.Contains(s, "=") }

	branchStart, checked := 0, 0
	for i, a := range args {
		if !isRef(a) {
			continue
		}
		if strings.HasPrefix(a, "mxfmux") {
			checked++
			for j := branchStart; j < i; j++ {
				if strings.HasPrefix(args[j], "leaky=") && args[j] != "leaky=no" {
					t.Errorf("branch feeding %s contains %q; a dropped buffer here "+
						"eventually aborts the process in mxfmux's index assertion",
						a, args[j])
				}
			}
		}
		branchStart = i // this reference starts the next branch
	}
	if checked < 2 {
		t.Fatalf("expected to check both mxfmux inputs (video and audio), checked %d", checked)
	}
}

// requiredElements drives BOTH the startup preflight and what ship.sh bundles
// into the .app. An element used by buildPipeline but missing from that list is
// not a warning — it is a plugin absent from the shipped bundle, which fails
// only on a machine with no Homebrew to fall back to. That shipped once:
// x264enc and mxfmux were added with the MXF output and never added here.
func TestRequiredElementsCoversEveryElementUsed(t *testing.T) {
	used := map[string]bool{
		// video
		"srtsrc": true, "tsdemux": true, "queue": true, "tee": true,
		"videoconvert": true, "timecodestamper": true, "progressreport": true,
		// ProRes output
		"vtenc_prores": true, "qtmux": true, "filesink": true,
		// AVC-Intra output
		"x264enc": true, "h264parse": true, "mxfmux": true,
		// audio
		"audioconvert": true, "audioresample": true, "level": true, "fakesink": true,
		// preview
		"glimagesink": true, "videorate": true, "videoscale": true,
		"jpegenc": true, "multipartmux": true, "fdsink": true,
	}
	have := map[string]bool{}
	for _, e := range requiredElements {
		have[e] = true
	}
	for el := range used {
		if !have[el] {
			t.Errorf("%q is used by buildPipeline but missing from requiredElements — "+
				"it will not be bundled into the .app", el)
		}
	}
}
