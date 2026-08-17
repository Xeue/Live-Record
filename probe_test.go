package main

import (
	"os"
	"testing"
)

// testdata/sample.ts is a real capture from an SRT feed carrying H.264 video
// and AAC audio. Its first PMT is a stale version-0 table listing audio only,
// which is exactly the case that made the parser report a video feed as
// audio-only and refuse to record it.
func TestParsePMT(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.ts")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}

	info, err := parsePMT(data)
	if err != nil {
		t.Fatalf("parsePMT: %v", err)
	}
	if !info.HasVideo() {
		t.Error("video not found; the stale first PMT is winning again")
	}
	if !info.HasAudio() {
		t.Error("audio not found")
	}
	if got, want := info.VideoCaps, "video/x-h264"; got != want {
		t.Errorf("VideoCaps = %q, want %q", got, want)
	}
	if got, want := info.VideoParser, "h264parse"; got != want {
		t.Errorf("VideoParser = %q, want %q", got, want)
	}
	if got, want := info.AudioCaps, "audio/mpeg"; got != want {
		t.Errorf("AudioCaps = %q, want %q", got, want)
	}
	if got, want := info.AudioParser, "aacparse"; got != want {
		t.Errorf("AudioParser = %q, want %q", got, want)
	}
	if info.ProgramNumber <= 0 {
		t.Errorf("ProgramNumber = %d, want a real program number", info.ProgramNumber)
	}
}

// A stream carrying two programs must resolve to exactly one, and it must be
// one that actually has video. Getting this wrong records the wrong program's
// audio silently, which is how it was found in the first place.
func TestParsePMTPicksProgramWithVideo(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.ts")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	info, err := parsePMT(data)
	if err != nil {
		t.Fatalf("parsePMT: %v", err)
	}
	if !info.HasVideo() {
		t.Fatal("chosen program has no video")
	}

	// Same bytes, parsed twice, must choose the same program: the selection has
	// to be deterministic rather than dependent on map iteration order.
	for i := 0; i < 20; i++ {
		again, err := parsePMT(data)
		if err != nil {
			t.Fatalf("parsePMT run %d: %v", i, err)
		}
		if again.ProgramNumber != info.ProgramNumber {
			t.Fatalf("run %d chose program %d, first run chose %d — selection is not deterministic",
				i, again.ProgramNumber, info.ProgramNumber)
		}
	}
}

func TestParsePMTRejectsGarbage(t *testing.T) {
	if _, err := parsePMT(make([]byte, 4096)); err == nil {
		t.Error("expected an error for a buffer with no TS sync bytes")
	}
	if _, err := parsePMT([]byte{0x47}); err == nil {
		t.Error("expected an error for a truncated stream")
	}
}

// A feed with no audio must still be recordable: the pipeline just omits the
// muxer's audio pad rather than waiting forever for data on it.
func TestStreamInfoVideoOnly(t *testing.T) {
	si := StreamInfo{VideoCaps: "video/x-h264", VideoParser: "h264parse", VideoDec: "vtdec_hw"}
	if !si.HasVideo() {
		t.Error("HasVideo should be true")
	}
	if si.HasAudio() {
		t.Error("HasAudio should be false")
	}
}
