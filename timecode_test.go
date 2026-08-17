package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// atom assembles a QuickTime atom: 4-byte size, 4-byte type, body.
func atom(typ string, body ...[]byte) []byte {
	var payload []byte
	for _, b := range body {
		payload = append(payload, b...)
	}
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	return append(out, payload...)
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

// buildMOV writes a file shaped like the ones qtmux produces in robust mode:
// ftyp, moov (containing a tmcd trak whose chunk offset is the faulty one), and
// mdat holding the timecode sample. mdatPad controls where the payload starts.
func buildMOV(t *testing.T, stcoValue uint32, sample uint32, mdatPad int) (path string, mdatPayload int64) {
	t.Helper()

	stsd := atom("stsd", u32(0), u32(1), atom("tmcd"))
	stco := atom("stco", u32(0), u32(1), u32(stcoValue))
	stbl := atom("stbl", stsd, stco)
	minf := atom("minf", stbl)
	mdia := atom("mdia", minf)
	trak := atom("trak", mdia)
	moov := atom("moov", trak)

	ftyp := atom("ftyp", []byte("qt  "))
	// A free atom stands in for the reserved index space; the faulty offset
	// points into it, which on a real recording is a hole reading as zeros.
	free := atom("free", make([]byte, mdatPad))

	// The mdat holds the timecode sample at stcoValue bytes into the payload,
	// with room after it, as a real interleaved recording would.
	payload := make([]byte, int(stcoValue)+64)
	binary.BigEndian.PutUint32(payload[stcoValue:], sample)
	mdat := atom("mdat", payload)

	buf := append(append(append(append([]byte{}, ftyp...), free...), moov...), mdat...)
	mdatPayload = int64(len(ftyp) + len(free) + len(moov) + 8)

	path = filepath.Join(t.TempDir(), "test.mov")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, mdatPayload
}

func readSTCO(t *testing.T, path string) uint32 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+12 < len(data); i++ {
		if string(data[i:i+4]) == "stco" {
			return binary.BigEndian.Uint32(data[i+12 : i+16])
		}
	}
	t.Fatal("no stco found")
	return 0
}

// The real defect: the timecode track's offset was never rebased, so it points
// before mdat, into the reserved header.
func TestRepairTimecodeOffset(t *testing.T) {
	const relative = 4 // offset relative to the mdat payload
	path, mdatPayload := buildMOV(t, relative, 1681011, 512)

	fixed, err := repairTimecodeOffset(path)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if !fixed {
		t.Fatal("expected a repair to be written")
	}
	got, want := readSTCO(t, path), uint32(int64(relative)+mdatPayload)
	if got != want {
		t.Errorf("stco = %d, want %d (relative %d + mdat payload %d)",
			got, want, relative, mdatPayload)
	}
	if int64(got) < mdatPayload {
		t.Errorf("stco %d still points before the mdat payload at %d", got, mdatPayload)
	}
}

// It runs once a second against a growing file, so applying it to an
// already-correct file must change nothing.
func TestRepairTimecodeOffsetIsIdempotent(t *testing.T) {
	path, _ := buildMOV(t, 4, 1681011, 512)

	if fixed, err := repairTimecodeOffset(path); err != nil || !fixed {
		t.Fatalf("first pass: fixed=%v err=%v", fixed, err)
	}
	after := readSTCO(t, path)

	for i := 0; i < 5; i++ {
		fixed, err := repairTimecodeOffset(path)
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if fixed {
			t.Fatalf("pass %d reported a repair on an already-correct file", i)
		}
		if got := readSTCO(t, path); got != after {
			t.Fatalf("pass %d changed the offset from %d to %d", i, after, got)
		}
	}
}

// A file whose offsets are already inside mdat — a plain, non-robust recording
// — must be left completely alone.
func TestRepairTimecodeOffsetLeavesGoodFilesAlone(t *testing.T) {
	path, mdatPayload := buildMOV(t, 0, 1681011, 512)
	// Rewrite the offset to a correct absolute value first.
	data, _ := os.ReadFile(path)
	for i := 0; i+16 < len(data); i++ {
		if string(data[i:i+4]) == "stco" {
			binary.BigEndian.PutUint32(data[i+12:i+16], uint32(mdatPayload))
			break
		}
	}
	os.WriteFile(path, data, 0o644)

	fixed, err := repairTimecodeOffset(path)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if fixed {
		t.Error("reported a repair on a file that was already correct")
	}
	if got := readSTCO(t, path); int64(got) != mdatPayload {
		t.Errorf("stco = %d, want %d unchanged", got, mdatPayload)
	}
}

// A file with no timecode track at all must not error.
func TestRepairTimecodeOffsetNoTimecodeTrack(t *testing.T) {
	stsd := atom("stsd", u32(0), u32(1), atom("apch"))
	stco := atom("stco", u32(0), u32(1), u32(4))
	moov := atom("moov", atom("trak", atom("mdia", atom("minf", atom("stbl", stsd, stco)))))
	buf := append(append(atom("ftyp", []byte("qt  ")), moov...), atom("mdat", u32(0))...)

	path := filepath.Join(t.TempDir(), "novt.mov")
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	fixed, err := repairTimecodeOffset(path)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if fixed {
		t.Error("repaired a file with no timecode track")
	}
}

// Truncated and malformed files turn up whenever a recording is interrupted;
// the repair must refuse them rather than panic or corrupt anything.
func TestRepairTimecodeOffsetRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"empty":     {},
		"tiny":      {0, 0, 0, 1},
		"badsize":   append(u32(3), []byte("moov")...), // size smaller than a header
		"truncated": append(atom("ftyp", []byte("qt  ")), 0x00, 0x00),
	}
	for name, body := range cases {
		path := filepath.Join(dir, name+".mov")
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		// Must return an error rather than panic; must never report a write.
		fixed, _ := repairTimecodeOffset(path)
		if fixed {
			t.Errorf("%s: reported a repair on a malformed file", name)
		}
	}
}
