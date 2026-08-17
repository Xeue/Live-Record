package main

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// movDuration reads the duration out of a QuickTime file's mvhd atom.
//
// Because we mux in robust-recording mode the moov sits at the *front* of the
// file and is rewritten every MoovUpdateSec, so this is cheap (a few hundred
// bytes read) and it reports exactly the duration Premiere will see when it
// re-reads the growing file. That makes it a far better progress signal than
// wall-clock time, which drifts away from reality whenever frames are dropped.
func movDuration(path string) (float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	moovOff, moovSize, err := findAtom(f, 0, 0, "moov")
	if err != nil {
		return 0, err
	}
	// mvhd is the first child of moov.
	mvhdOff, _, err := findAtom(f, moovOff+8, moovOff+moovSize, "mvhd")
	if err != nil {
		return 0, err
	}

	var hdr [32]byte
	if _, err := f.ReadAt(hdr[:], mvhdOff+8); err != nil {
		return 0, err
	}
	version := hdr[0]

	var timescale, duration uint64
	switch version {
	case 0:
		// version(1) flags(3) created(4) modified(4) timescale(4) duration(4)
		timescale = uint64(binary.BigEndian.Uint32(hdr[12:16]))
		duration = uint64(binary.BigEndian.Uint32(hdr[16:20]))
	case 1:
		// version(1) flags(3) created(8) modified(8) timescale(4) duration(8)
		timescale = uint64(binary.BigEndian.Uint32(hdr[20:24]))
		duration = binary.BigEndian.Uint64(hdr[24:32])
	default:
		return 0, errors.New("unsupported mvhd version")
	}
	if timescale == 0 {
		return 0, errors.New("mvhd timescale is zero")
	}
	// 0xFFFFFFFF is the placeholder qtmux writes before the first index update.
	if version == 0 && duration == 0xFFFFFFFF {
		return 0, nil
	}
	return float64(duration) / float64(timescale), nil
}

// findAtom scans sibling atoms from start until end (end==0 means EOF) and
// returns the offset and size of the first one matching typ.
func findAtom(f *os.File, start, end int64, typ string) (off, size int64, err error) {
	if end == 0 {
		st, err := f.Stat()
		if err != nil {
			return 0, 0, err
		}
		end = st.Size()
	}
	pos := start
	for pos+8 <= end {
		var hdr [8]byte
		if _, err := f.ReadAt(hdr[:], pos); err != nil {
			if err == io.EOF {
				break
			}
			return 0, 0, err
		}
		sz := int64(binary.BigEndian.Uint32(hdr[0:4]))
		name := string(hdr[4:8])

		switch sz {
		case 1: // 64-bit extended size follows the header
			var ext [8]byte
			if _, err := f.ReadAt(ext[:], pos+8); err != nil {
				return 0, 0, err
			}
			sz = int64(binary.BigEndian.Uint64(ext[:]))
		case 0: // atom runs to end of file (this is our growing mdat)
			sz = end - pos
		}
		if sz < 8 {
			return 0, 0, errors.New("corrupt atom size")
		}
		if name == typ {
			return pos, sz, nil
		}
		pos += sz
	}
	return 0, 0, errors.New(typ + " atom not found")
}
