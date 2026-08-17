package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// repairTimecodeOffset fixes the timecode track's chunk offset in a QuickTime
// file written by qtmux in robust recording mode.
//
// # The bug
//
// qtmux stores every track's chunk offsets relative to the start of the mdat
// payload and rebases them, once, in atom_moov_chunks_set_offset(). That
// function memoises: if the base has not changed it returns immediately. The
// base never changes in robust mode, so the rebase happens exactly once — on
// the first sample registered by any pad.
//
// The timecode track is created lazily, when the first video buffer carrying a
// timecode meta reaches the muxer. With audio present, audio samples register
// first (video waits for an IDR, then decode and ProRes encode), so the one and
// only rebase runs before the timecode track exists, and atom_moov_add_trak()
// does not inherit the current base. Its offset stays relative forever.
//
// The result points into the reserved header, which is a file hole reading as
// zeros, so the timecode decodes as 00:00:00:00 — even though the correct value
// was written into the mdat all along. Verified on this machine: audio and
// video offsets landed inside mdat, the timecode track's did not, and adding
// the mdat payload start to it revealed the true time of day.
//
// This is unreported upstream and still present on GStreamer main, so we repair
// the file rather than wait for a fix. The repair is a single 32- or 64-bit
// field, and it is idempotent: an already-correct offset is left alone.
//
// Returns true if a repair was written.
func repairTimecodeOffset(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := st.Size()

	mdatPayload, moovStart, moovEnd, err := locateTopLevel(f, size)
	if err != nil {
		return false, err
	}
	if mdatPayload <= 0 {
		return false, errors.New("no mdat found")
	}

	// moov children: find the trak whose sample description is 'tmcd'.
	for _, trak := range childAtoms(f, moovStart, moovEnd, "trak") {
		mdia := firstChild(f, trak.body, trak.end, "mdia")
		if mdia == nil {
			continue
		}
		minf := firstChild(f, mdia.body, mdia.end, "minf")
		if minf == nil {
			continue
		}
		stbl := firstChild(f, minf.body, minf.end, "stbl")
		if stbl == nil {
			continue
		}
		stsd := firstChild(f, stbl.body, stbl.end, "stsd")
		if stsd == nil {
			continue
		}
		// stsd: version+flags(4) entry_count(4) then entry_size(4) format(4)
		var fmtBuf [4]byte
		if _, err := f.ReadAt(fmtBuf[:], stsd.body+12); err != nil {
			continue
		}
		if string(fmtBuf[:]) != "tmcd" {
			continue
		}
		return repairChunkOffsets(f, stbl, mdatPayload, size)
	}
	return false, nil // no timecode track: nothing to do
}

func repairChunkOffsets(f *os.File, stbl *atomRef, mdatPayload, size int64) (bool, error) {
	if stco := firstChild(f, stbl.body, stbl.end, "stco"); stco != nil {
		return rewriteOffsets(f, stco.body, mdatPayload, size, 4)
	}
	if co64 := firstChild(f, stbl.body, stbl.end, "co64"); co64 != nil {
		return rewriteOffsets(f, co64.body, mdatPayload, size, 8)
	}
	return false, nil
}

// rewriteOffsets adds the mdat payload start to any entry that still points
// before it. Entries already inside mdat are correct and left untouched, which
// makes this safe to run repeatedly on a growing file.
func rewriteOffsets(f *os.File, body, mdatPayload, size int64, width int) (bool, error) {
	var hdr [8]byte // version+flags(4) entry_count(4)
	if _, err := f.ReadAt(hdr[:], body); err != nil {
		return false, err
	}
	count := int64(binary.BigEndian.Uint32(hdr[4:8]))
	if count <= 0 || count > 1<<20 {
		return false, nil
	}

	changed := false
	for i := int64(0); i < count; i++ {
		at := body + 8 + i*int64(width)
		buf := make([]byte, width)
		if _, err := f.ReadAt(buf, at); err != nil {
			return changed, err
		}
		var cur int64
		if width == 4 {
			cur = int64(binary.BigEndian.Uint32(buf))
		} else {
			cur = int64(binary.BigEndian.Uint64(buf))
		}
		if cur >= mdatPayload {
			continue // already rebased
		}
		fixed := cur + mdatPayload
		if fixed < mdatPayload || fixed+4 > size {
			return changed, fmt.Errorf("refusing to write chunk offset %d: outside the file", fixed)
		}
		if width == 4 {
			if fixed > 0xFFFFFFFF {
				return changed, errors.New("chunk offset needs 64 bits but the table is 32-bit")
			}
			binary.BigEndian.PutUint32(buf, uint32(fixed))
		} else {
			binary.BigEndian.PutUint64(buf, uint64(fixed))
		}
		if _, err := f.WriteAt(buf, at); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// ---------------------------------------------------------------------------
// minimal atom walking

type atomRef struct {
	typ  string
	body int64 // first byte after the header
	end  int64 // one past the last byte of the atom
}

func readAtomHeader(f *os.File, pos, end int64) (*atomRef, int64, error) {
	if pos+8 > end {
		return nil, 0, errors.New("eof")
	}
	var h [8]byte
	if _, err := f.ReadAt(h[:], pos); err != nil {
		return nil, 0, err
	}
	size := int64(binary.BigEndian.Uint32(h[0:4]))
	typ := string(h[4:8])
	body := pos + 8
	switch size {
	case 1:
		var ext [8]byte
		if _, err := f.ReadAt(ext[:], pos+8); err != nil {
			return nil, 0, err
		}
		size = int64(binary.BigEndian.Uint64(ext[:]))
		body = pos + 16
	case 0:
		size = end - pos // runs to the end of the file: our growing mdat
	}
	if size < 8 || pos+size > end {
		return nil, 0, errors.New("corrupt atom size")
	}
	return &atomRef{typ: typ, body: body, end: pos + size}, pos + size, nil
}

func childAtoms(f *os.File, start, end int64, typ string) []*atomRef {
	var out []*atomRef
	for pos := start; pos < end; {
		a, next, err := readAtomHeader(f, pos, end)
		if err != nil {
			break
		}
		if a.typ == typ {
			out = append(out, a)
		}
		pos = next
	}
	return out
}

func firstChild(f *os.File, start, end int64, typ string) *atomRef {
	if c := childAtoms(f, start, end, typ); len(c) > 0 {
		return c[0]
	}
	return nil
}

// locateTopLevel returns the mdat payload start and the moov bounds.
func locateTopLevel(f *os.File, size int64) (mdatPayload, moovStart, moovEnd int64, err error) {
	for pos := int64(0); pos < size; {
		a, next, e := readAtomHeader(f, pos, size)
		if e != nil {
			break
		}
		switch a.typ {
		case "mdat":
			mdatPayload = a.body
		case "moov":
			moovStart, moovEnd = a.body, a.end
		}
		pos = next
	}
	if moovStart == 0 {
		return 0, 0, 0, errors.New("no moov found")
	}
	return mdatPayload, moovStart, moovEnd, nil
}
