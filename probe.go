package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// StreamInfo is what a feed actually carries, read from the transport stream's
// own program map rather than guessed.
//
// This exists because auto-plugging (decodebin) is unreliable on a live feed
// joined mid-GOP: it waits for buffers on every stream it thinks exists and
// gives up with "all streams without buffers", leaving a pipeline that is up
// but silently producing nothing. Reading the PMT tells us exactly which
// branches to build, so the pipeline links deterministically.
// probeTimeout bounds how long we wait for a program map. A PMT repeats several
// times a second on any sane encoder, so reaching this means the sender is not
// there, is not sending MPEG-TS, or the SRT parameters are wrong.
const probeTimeout = 8 * time.Second

type StreamInfo struct {
	// ProgramNumber is the MPEG-TS program we record. The demuxer is pinned to
	// it, because a stream carrying more than one program exposes several audio
	// pads with identical caps and a caps-routed link then picks one at random
	// — which silently records the wrong program's audio.
	ProgramNumber int

	VideoCaps   string // caps that route the demuxer's video pad
	VideoParser string
	VideoDec    string
	AudioCaps   string
	AudioParser string
	AudioDec    string
}

func (s StreamInfo) HasVideo() bool { return s.VideoCaps != "" }
func (s StreamInfo) HasAudio() bool { return s.AudioCaps != "" }

func (s StreamInfo) String() string {
	v, a := "none", "none"
	if s.HasVideo() {
		v = s.VideoCaps
	}
	if s.HasAudio() {
		a = s.AudioCaps
	}
	return fmt.Sprintf("program=%d video=%s audio=%s", s.ProgramNumber, v, a)
}

// MPEG-TS stream_type values we can handle (ISO/IEC 13818-1 Table 2-34).
func videoForType(t byte) (caps, parser, dec string, ok bool) {
	switch t {
	case 0x01, 0x02:
		return "video/mpeg", "mpegvideoparse", "avdec_mpeg2video", true
	case 0x1B:
		return "video/x-h264", "h264parse", "vtdec_hw", true
	case 0x24:
		return "video/x-h265", "h265parse", "vtdec_hw", true
	}
	return "", "", "", false
}

// audioForType maps a stream type to an audio branch. desc is the elementary
// stream's descriptor block, which is the only thing that distinguishes AC-3
// from the other things that share stream type 0x06.
func audioForType(t byte, desc []byte) (caps, parser, dec string, ok bool) {
	switch t {
	case 0x03, 0x04:
		// mpegversion pins this to MPEG-1/2 audio so it cannot capture an AAC
		// pad, which also advertises audio/mpeg.
		return "audio/mpeg,mpegversion=(int)1", "mpegaudioparse", "avdec_mp3", true
	case 0x0F, 0x11:
		return "audio/mpeg", "aacparse", "avdec_aac", true
	case 0x81:
		return "audio/x-ac3", "ac3parse", "avdec_ac3", true // ATSC AC-3
	case 0x06:
		// 0x06 is "PES private data". In DVB it carries AC-3 only when an AC-3
		// descriptor says so; otherwise it is teletext or subtitles. Guessing
		// audio here requests a muxer pad that never receives data, and the
		// muxer then blocks forever while the UI shows a healthy recording.
		if hasAC3Descriptor(desc) {
			return "audio/x-ac3", "ac3parse", "avdec_ac3", true
		}
	}
	return "", "", "", false
}

// hasAC3Descriptor looks for the DVB AC-3 (0x6A) or E-AC-3 (0x7A) descriptor,
// or the ATSC registration of the same.
func hasAC3Descriptor(desc []byte) bool {
	for i := 0; i+2 <= len(desc); {
		tag, dlen := desc[i], int(desc[i+1])
		if tag == 0x6A || tag == 0x7A {
			return true
		}
		if tag == 0x05 && i+2+4 <= len(desc) { // registration_descriptor
			switch string(desc[i+2 : i+6]) {
			case "AC-3", "BSSD", "EAC3":
				return true
			}
		}
		i += 2 + dlen
	}
	return false
}

// probe dials the feed briefly, captures raw transport stream to a temp file
// and reads its PAT/PMT. It is codec-agnostic and works regardless of where in
// the GOP we happen to join, because the PMT is repeated continuously and does
// not depend on having seen a keyframe.
func (f *Feed) probe(ctx context.Context) (StreamInfo, error) {
	tmp, err := os.CreateTemp("", "liverecord-probe-*.ts")
	if err != nil {
		return StreamInfo{}, err
	}
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)

	args := []string{"-q",
		"srtsrc", "uri=" + gstQuote(f.srtURI()), "latency=" + fmt.Sprint(f.cfg.Latency),
	}
	if f.cfg.StreamID != "" {
		args = append(args, "streamid="+gstQuote(f.cfg.StreamID))
	}
	if f.cfg.Passphrase != "" {
		args = append(args, "passphrase="+gstQuote(f.cfg.Passphrase))
	}
	args = append(args, "!", "filesink", "location="+gstQuote(path))

	cmd := exec.Command(f.app.gstLaunch, args...)
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return StreamInfo{}, err
	}
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	// Poll the capture and stop the instant the program map is readable, rather
	// than capturing a fixed number of packets.
	//
	// A packet count is a proxy for time only at a fixed bitrate. Measured on
	// two test feeds differing only in how compressible their picture was, a
	// fixed 800-packet probe finished 7 seconds apart — which on a real
	// multi-camera record would start the ISOs at visibly different moments.
	// Waiting for video specifically also skips the stale audio-only PMT a
	// muxer emits before all its streams are registered.
	deadline := time.After(probeTimeout)
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()

	var last StreamInfo
	for {
		select {
		case <-ctx.Done():
			return StreamInfo{}, ctx.Err()

		case <-done:
			// The source ended on its own; parse whatever landed.
			data, err := os.ReadFile(path)
			if err != nil || len(data) < 188 {
				return StreamInfo{}, errors.New("no data received from feed")
			}
			return parsePMT(data)

		case <-deadline:
			if last.HasVideo() || last.HasAudio() {
				return last, nil
			}
			return StreamInfo{}, fmt.Errorf(
				"no program map after %s — is the sender running, and is it MPEG-TS?", probeTimeout)

		case <-tick.C:
			data, err := os.ReadFile(path)
			if err != nil || len(data) < 188*4 {
				continue
			}
			info, err := parsePMT(data)
			if err != nil {
				continue
			}
			last = info
			if info.HasVideo() {
				return info, nil
			}
		}
	}
}

// parsePMT walks the transport stream looking for the PAT, then the PMT it
// points at, and maps the elementary stream types it lists.
//
// Every PMT in the capture is parsed and the last one wins, rather than
// returning on the first. A muxer typically emits an initial PMT before all
// its streams are registered — mpegtsmux sends a version-0 table listing audio
// only, then a version-1 table with audio and video — so stopping at the first
// match reports a video feed as audio-only.
func parsePMT(data []byte) (info StreamInfo, err error) {
	// Belt and braces alongside the bounds checks below: this parses bytes off
	// the network, and a panic here would kill every feed in the process, not
	// just this one, leaving orphaned gst-launch children still writing.
	defer func() {
		if r := recover(); r != nil {
			info, err = StreamInfo{}, fmt.Errorf("malformed transport stream: %v", r)
		}
	}()

	const pktLen = 188

	// Find packet alignment: the sync byte 0x47 every 188 bytes.
	start := -1
	for i := 0; i+pktLen*3 < len(data) && i < pktLen*2; i++ {
		if data[i] == 0x47 && data[i+pktLen] == 0x47 && data[i+pktLen*2] == 0x47 {
			start = i
			break
		}
	}
	if start < 0 {
		return StreamInfo{}, errors.New("not a valid MPEG-TS stream")
	}

	// A stream may advertise several programs. Collect them all and choose
	// afterwards, rather than latching onto whichever appears first.
	pmtPIDs := map[uint16]bool{}   // PIDs carrying a program map
	order := []int{}               // program numbers, in PAT order
	byProgram := map[int]StreamInfo{}

	for off := start; off+pktLen <= len(data); off += pktLen {
		pkt := data[off : off+pktLen]
		if pkt[0] != 0x47 {
			continue
		}
		// A section only begins in a packet with payload_unit_start_indicator
		// set. Continuation packets carry section *body*, and parsing one as a
		// header yields convincing nonsense.
		if pkt[1]&0x40 == 0 {
			continue
		}
		pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
		if pid != 0 && !pmtPIDs[pid] {
			continue
		}
		adaptation := (pkt[3] >> 4) & 0x3
		if adaptation == 0 || adaptation == 2 {
			continue // no payload
		}
		p := pkt[4:]
		if adaptation == 3 { // adaptation field precedes the payload
			if len(p) < 1 {
				continue
			}
			alen := int(p[0]) + 1
			if alen >= len(p) {
				continue
			}
			p = p[alen:]
		}
		if len(p) < 1 {
			continue
		}
		// pointer_field is 0-255 but the remaining payload can be as little as
		// one byte. An unchecked skip here panics on a bit-flipped packet or a
		// false sync lock, and a panic on this goroutine would take down every
		// other feed's recording with it.
		ptr := 1 + int(p[0])
		if ptr >= len(p) {
			continue
		}
		p = p[ptr:]
		if len(p) < 12 {
			continue
		}
		sectionLen := int(p[1]&0x0F)<<8 | int(p[2])
		if sectionLen+3 > len(p) {
			continue // section spans packets; a later repeat will fit
		}
		end := sectionLen + 3 - 4 // stop before the CRC

		if pid == 0 { // PAT: every program and the PID carrying its map
			for i := 8; i+4 <= end; i += 4 {
				progNum := int(p[i])<<8 | int(p[i+1])
				pidVal := uint16(p[i+2]&0x1F)<<8 | uint16(p[i+3])
				if progNum == 0 {
					continue // program 0 is the network PID, not a program
				}
				if _, seen := byProgram[progNum]; !seen {
					byProgram[progNum] = StreamInfo{ProgramNumber: progNum}
					order = append(order, progNum)
				}
				pmtPIDs[pidVal] = true
			}
			continue
		}

		// PMT. Rebuild from scratch each time so the newest table for a program
		// wins outright rather than merging with a stale one.
		progNum := int(p[3])<<8 | int(p[4])
		progInfoLen := int(p[10]&0x0F)<<8 | int(p[11])
		cur := StreamInfo{ProgramNumber: progNum}
		for i := 12 + progInfoLen; i+5 <= end; {
			streamType := p[i]
			esInfoLen := int(p[i+3]&0x0F)<<8 | int(p[i+4])
			descEnd := i + 5 + esInfoLen
			if descEnd > end {
				descEnd = end // truncated table: use what is there
			}
			desc := p[i+5 : descEnd]

			if caps, parser, dec, ok := videoForType(streamType); ok && !cur.HasVideo() {
				cur.VideoCaps, cur.VideoParser, cur.VideoDec = caps, parser, dec
			}
			if caps, parser, dec, ok := audioForType(streamType, desc); ok && !cur.HasAudio() {
				cur.AudioCaps, cur.AudioParser, cur.AudioDec = caps, parser, dec
			}
			i += 5 + esInfoLen
		}
		if cur.HasVideo() || cur.HasAudio() {
			if _, seen := byProgram[progNum]; !seen {
				order = append(order, progNum)
			}
			byProgram[progNum] = cur
		}
	}

	if len(byProgram) == 0 {
		return StreamInfo{}, errors.New("no program map found in the stream")
	}

	// Prefer the first program that carries video and audio, then the first
	// with video, then anything. We are a video recorder: a program with no
	// video is never the one wanted.
	var best StreamInfo
	for _, rank := range []func(StreamInfo) bool{
		func(s StreamInfo) bool { return s.HasVideo() && s.HasAudio() },
		func(s StreamInfo) bool { return s.HasVideo() },
		func(s StreamInfo) bool { return s.HasAudio() },
	} {
		for _, pn := range order {
			if s := byProgram[pn]; rank(s) {
				return s, nil
			}
		}
	}
	return best, errors.New("no usable audio or video found in the feed")
}
