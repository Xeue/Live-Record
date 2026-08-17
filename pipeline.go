package main

import (
	"fmt"
	"strconv"
	"strings"
)

// gstQuote wraps a property value so gst-launch's parser keeps it intact.
//
// gst-launch joins its whole argv into one string before parsing, so any value
// containing a space (an output directory under ~/Movies, say) would otherwise
// be split into separate tokens. The parser understands C-style quoted strings.
func gstQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (f *Feed) srtURI() string {
	return fmt.Sprintf("srt://%s:%d?mode=caller", f.cfg.Host, f.cfg.Port)
}

// buildPipeline produces the gst-launch argument vector for one feed.
//
//	srtsrc(caller) -> tsdemux -+-> parse -> decode -> tee -+-> vtenc_prores -> qtmux -> file
//	                           |                           `-> scale -> jpegenc -> fd 3
//	                           `-> parse -> decode -> level -> PCM24 -> qtmux
//
// The demuxer's pads are routed by the exact caps the probe found in the PMT,
// so every link is resolved deterministically. Monitor mode is the same minus
// the ProRes/qtmux branch.
// previewFD is where the browser preview's JPEG stream is written. The
// subprocess engine passes fd 3 (supplied via cmd.ExtraFiles); the in-process
// engine passes the real write end of a pipe it owns.
func (f *Feed) buildPipeline(want State, outFile, mxfFile string, si StreamInfo, previewFD int) []string {
	c := f.appCfg
	// noPreroll is appended to EVERY sink in this pipeline, and it is what makes
	// two output formats on one feed possible at all.
	//
	// By default a GstBaseSink completes its READY->PAUSED transition only once
	// it has received a buffer, and a GstBin will not move ANY child on to
	// PLAYING until EVERY async child has finished. So the pipeline can only
	// leave PAUSED after all of its sinks have seen data.
	//
	// That is a deadlock as soon as one sink's data depends on the others
	// draining. Measured, with GST_DEBUG=GST_STATES:5 and a thread sample of the
	// wedged process:
	//
	//	 - mxfmux's filesink cannot preroll until x264enc emits its first frame,
	//	   and x264enc holds a dozen-odd frames before it emits anything;
	//	 - meanwhile the ProRes branch's queues fill, because ITS filesink has
	//	   prerolled and is parked in gst_base_sink_wait_preroll waiting for a
	//	   PLAYING that cannot arrive;
	//	 - a full queue back-pressures the tee, which stops the decoder, so
	//	   x264enc never gets the frames it needs. Nothing moves again, ever.
	//
	// The decode thread then wedges for good on top of that: GST_QUERY_ALLOCATION
	// is a SERIALIZED query, so vtdec's pool negotiation is queued behind the
	// stuck buffers in the preview queue and never returns.
	//
	// async=false takes the sinks out of that accounting entirely: they never
	// gate the state change and never park in wait_preroll. Preroll buys a live
	// recorder nothing anyway — there is no frame to show while PAUSED, because
	// a live source produces nothing until PLAYING.
	//
	// This is why the failure looked like an in-process-only bug: it is a race
	// between "how long the slowest sink takes to preroll" and "how much the
	// other branches can buffer meanwhile", and glimagesink's GL context takes
	// ~1.2s to come up, against microseconds for the fdsink the CLI uses.
	const noPreroll = "async=false"
	// Element names are suffixed per feed. In the subprocess engine each
	// pipeline owns its process, so plain names like "d" and "mux" cannot
	// collide. In-process they can: gst_parse_launch resolves "d." references by
	// NAME, so with two feeds parsed into one address space the second
	// pipeline's references can bind to the first pipeline's elements, leaving
	// its own demuxer unlinked and that feed silently recording nothing.
	// Measured exactly that: "gst_parse_no_more_pads:<d> Delayed linking failed",
	// first feed fine, second feed zero bytes.
	n := f.elementSuffix()
	recording := want == StateRecording && c.RecordProRes
	// The MXF exists because Premiere only treats growing MXF as growing media:
	// measured, an AVC-Intra MXF goes italic and self-updates on the refresh
	// timer, while a growing ProRes QuickTime never does. It is written from the
	// same decoded frames as the ProRes, off the same tee.
	mxfRecording := want == StateRecording && c.RecordAVCIntra
	// Audio is only muxed if the feed actually carries some. A muxer blocks
	// until every one of its pads has data, so requesting an audio pad for a
	// video-only feed would mean nothing was ever written.
	muxAudio := recording && si.HasAudio()

	var a []string
	add := func(parts ...string) { a = append(a, parts...) }

	// -e: push EOS on SIGINT so the moov is finalised cleanly.
	// -m: print bus messages so we can read audio levels and the heartbeat.
	add("-e", "-m")

	// ---- source -------------------------------------------------------
	add("srtsrc", "name=src"+n,
		"uri="+gstQuote(f.srtURI()),
		"latency="+strconv.Itoa(f.cfg.Latency),
		"auto-reconnect=true",
		// Without this a momentary drop ends the segment and tears the
		// pipeline (and the recording) down.
		"automatic-eos=false")
	if f.cfg.StreamID != "" {
		add("streamid=" + gstQuote(f.cfg.StreamID))
	}
	if f.cfg.Passphrase != "" {
		add("passphrase=" + gstQuote(f.cfg.Passphrase))
	}

	// Bounded on purpose. All-zero means unlimited, and if the encoder or the
	// disk falls behind, this queue absorbs the whole incoming stream until the
	// process is killed for memory — losing every feed, with no EOS and no
	// finalised header. For a live recorder the right behaviour when we cannot
	// keep up is to let backpressure reach srtsrc and drop, not to buffer.
	add("!", "queue", "max-size-time=2000000000", "max-size-bytes=0", "max-size-buffers=0")


	// Pin the demuxer to the program the probe chose. Left to itself it exposes
	// pads for every program in the stream, and since several programs' audio
	// pads share identical caps, the caps-routed link below would attach to an
	// arbitrary one — recording the wrong program's audio with no error.
	add("!", "tsdemux", "name=d"+n, "program-number="+strconv.Itoa(si.ProgramNumber))

	// ---- video: demux -> parse -> decode -> heartbeat -> tee ------------
	add("d"+n+".", "!", si.VideoCaps, "!", "queue", "!", si.VideoParser, "!", si.VideoDec, "!", "videoconvert")
	// Stamp every frame with wall-clock time of day. In sports the operative
	// question is "what time did that happen", not "how far into the file is
	// it", so the timecode track carries TOD and the elapsed position is left
	// to the file's own duration.
	add("!", "timecodestamper", "source=rtc", "name=tc"+n)
	add("!", "progressreport", "update-freq=1", "silent=true", "!", "tee", "name=v"+n)

	// ---- audio ---------------------------------------------------------
	// Decoded once and, when both formats are being written, teed — the same
	// shape as the video path, so neither muxer waits on the other.
	audioTee := si.HasAudio() && muxAudio && mxfRecording
	if si.HasAudio() {
		add("d"+n+".", "!", si.AudioCaps, "!", "queue", "!", si.AudioParser, "!", si.AudioDec)
		add("!", "audioconvert", "!", "audioresample", "!", "level", "interval=200000000")
		switch {
		case audioTee:
			// Premiere is happiest with uncompressed PCM alongside the picture,
			// and both muxers take the same format, so convert once.
			add("!", "audioconvert", "!", "audio/x-raw,rate=48000,channels=2,format=S24BE")
			add("!", "tee", "name=a"+n)
			add("a"+n+".", "!", "queue", "!", "mux"+n+".audio_0")
			// Deep, but NOT leaky — see the MXF video queue below for why a
			// dropped buffer on an mxfmux pad is fatal. It only has to be deep
			// enough to cover x264enc's encode latency, since mxfmux cannot
			// output until both pads have data.
			add("a"+n+".", "!", "queue",
				"max-size-buffers=0", "max-size-bytes=0", "max-size-time=3000000000",
				"!", "mxfmux"+n+".")
		case muxAudio:
			add("!", "audioconvert", "!", "audio/x-raw,rate=48000,channels=2,format=S24BE")
			add("!", "queue", "!", "mux"+n+".audio_0")
		case mxfRecording:
			add("!", "audioconvert", "!", "audio/x-raw,rate=48000,channels=2,format=S24BE")
			add("!", "queue", "!", "mxfmux"+n+".")
		default:
			add("!", "fakesink", "sync=false", noPreroll)
		}
	}

	// ---- preview branches -----------------------------------------------
	// leaky=downstream keeps a slow or absent preview consumer from ever
	// applying backpressure to the recording branch.
	if c.nativePreview() {
		// Straight from the decoder to the GPU: no videorate, no videoscale, no
		// JPEG encode. Full resolution, full frame rate, and the only cost is
		// the blit that any preview has to do anyway.
		add("v"+n+".", "!", "queue", "leaky=downstream", "max-size-buffers=2")
		add("!", "glimagesink", "name=preview"+n, "sync=false", noPreroll, "force-aspect-ratio=true")
	}
	if c.browserPreview() {
		add("v"+n+".", "!", "queue", "leaky=downstream", "max-size-buffers=2")
		add("!", "videorate", "!", fmt.Sprintf("video/x-raw,framerate=%d/1", c.PreviewFPS))
		add("!", "videoscale", "!", fmt.Sprintf("video/x-raw,width=%d,height=%d,pixel-aspect-ratio=1/1",
			c.PreviewWidth, c.PreviewHeight))
		add("!", "videoconvert", "!", "jpegenc", "quality="+strconv.Itoa(c.PreviewQuality))
		add("!", "multipartmux", "boundary=frame")
		// fd 3 is a pipe supplied via cmd.ExtraFiles. Writing the preview to a
		// pipe rather than a TCP server sink means there is no port to collide
		// with, and no way for a preview failure to stop the pipeline
		// prerolling and take the recording down with it.
		add("!", "fdsink", "fd="+strconv.Itoa(previewFD), "sync=false", noPreroll)
	}
	if !c.nativePreview() && !c.browserPreview() {
		add("v"+n+".", "!", "queue", "leaky=downstream", "max-size-buffers=2",
			"!", "fakesink", "sync=false", noPreroll)
	}

	// ---- AVC-Intra MXF branch -------------------------------------------
	if mxfRecording {
		// Three seconds of slack so a burst of encoder slowness costs nothing,
		// and DELIBERATELY NOT LEAKY.
		//
		// This queue used to be leaky, on the theory that the MXF is the edit
		// copy and so the branch allowed to drop frames. That is wrong, and it
		// is worse than wrong — mxfmux cannot survive a dropped frame:
		//
		//	index_pos_diff = pts_pos - pad->pos;
		//	g_assert (index_pos_diff < 127 && index_pos_diff >= -127);
		//
		// pad->pos counts the edit units actually written; pts_pos is derived
		// from the timestamp. Every dropped frame widens the gap by one and it
		// never closes, so the 128th drop aborts the whole PROCESS — both feeds,
		// mid-take, with neither file finalised. Measured exactly that: a run
		// whose MXFs were 106 and 196 frames short of their ProRes masters died
		// on that assertion, and its two .mxf files have no readable duration.
		//
		// It was never buying anything anyway: the stall it was meant to cure
		// was the preroll deadlock described at the top of this function, and
		// making it leaky did not fix that. With the deadlock gone, x264enc
		// keeps up exactly — measured, 1464 and 1466 frames in the MXF against
		// 1464 and 1466 in the ProRes, i.e. not one frame dropped.
		//
		// If it ever genuinely cannot keep up, backpressure now reaches the tee
		// and the no-media watchdog reports a stall. Saying so is better than
		// silently writing a short MXF and then killing the app.
		add("v"+n+".", "!", "queue",
			"max-size-buffers=0", "max-size-bytes=0", "max-size-time=3000000000")
		add("!", "videoconvert", "!", "video/x-raw,format=I422_10LE")
		// x264 has native AVC-Intra support; GStreamer's x264enc does not expose
		// it as a property, but option-string is passed straight through. This
		// yields conformant High 4:2:2 Intra rather than merely similar-looking
		// H.264, which is what makes Premiere accept it as a supported format.
		add("!", "x264enc",
			"option-string="+gstQuote("avcintra-class="+strconv.Itoa(c.AVCIntraClass)))
		add("!", "h264parse", "!", "queue", "!", "mxfmux"+n+".")
		add("mxfmux", "name=mxfmux"+n)
		add("!", "filesink", "name=mxfsink"+n, "location="+gstQuote(mxfFile), "sync=false", noPreroll)
	}

	// ---- record branch --------------------------------------------------
	if recording {
		add("v"+n+".", "!", "queue", "!", "vtenc_prores")
		// The (string) type is not decoration: gst-launch infers a caps field's
		// type from the literal, so a bare variant=4444 becomes an int and can
		// never intersect with the encoder's string field. Every other flavour
		// contains a letter and happens to parse correctly, which is how this
		// would have survived to the one show that needed 4444.
		add("!", "video/x-prores,variant=(string)"+c.ProResVariant)
		add("!", "queue", "!", "mux"+n+".video_0")

		maxDurNs := uint64(c.MaxHours) * 3600 * 1e9
		updateNs := uint64(c.MoovUpdateSec) * 1e9
		add("qtmux", "name=mux"+n,
			// Robust recording mode: reserve index space up front and rewrite
			// the moov periodically, so the file on disk is a valid, playable
			// QuickTime movie at every instant rather than only at EOS.
			"reserved-max-duration="+strconv.FormatUint(maxDurNs, 10),
			"reserved-moov-update-period="+strconv.FormatUint(updateNs, 10),
			"reserved-bytes-per-sec="+strconv.Itoa(c.ReservedBytesPerSec),
			"reserved-prefill="+strconv.FormatBool(c.ReservedPrefill),
			// Write the tmcd track carrying the time-of-day stamps above, so
			// the TOD travels with the file into Premiere.
			"force-create-timecode-trak=true")
		add("!", "filesink", "name=sink"+n, "location="+gstQuote(outFile), "sync=false", noPreroll)
	}

	return a
}

// elementSuffix is a per-feed suffix for element names, so two pipelines can
// coexist in one process without their gst_parse_launch name references
// crossing. Letters and digits only: the parser treats most punctuation as
// syntax.
func (f *Feed) elementSuffix() string {
	var b strings.Builder
	b.WriteByte('_')
	for _, r := range f.cfg.Name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	fmt.Fprintf(&b, "%d", f.index)
	return b.String()
}
