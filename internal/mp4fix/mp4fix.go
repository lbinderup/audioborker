// Package mp4fix presents an MP4 whose 32-bit track length overflowed with
// corrected headers, so a browser plays — and seeks through — the whole book.
//
// A version-0 mdhd counts a track's length in 32 bits of its timescale; at
// 44.1 kHz that wraps past 27h03m. Browsers believe the header: a 27-hour
// book played as 2:29 and refused to seek further. The sample table still
// times every sample, so the true length is the sum of its stts entries.
// Inspect rebuilds the moov box with that length in a version-1 (64-bit)
// mdhd, which is 12 bytes longer, so chunk offsets pointing past the moov
// move by the same amount. Nothing is ever written to the file.
package mp4fix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Fix is the corrected view of one file: the original bytes with its moov
// box swapped for a rebuilt one.
type Fix struct {
	size               int64 // original file size
	moovStart, moovEnd int64 // the original moov's byte range
	moov               []byte
}

// Size is the corrected file's length.
func (f *Fix) Size() int64 { return f.size + f.delta() }

func (f *Fix) delta() int64 { return int64(len(f.moov)) - (f.moovEnd - f.moovStart) }

// Reader serves the corrected file over r, the original.
func (f *Fix) Reader(r io.ReaderAt) io.ReadSeeker { return &reader{r: r, f: f} }

var errMalformed = errors.New("mp4fix: malformed box structure")

// node is one box: its payload range in the source, and either children
// (containers on the path to the boxes this package edits) or a replacement
// payload.
type node struct {
	typ      string
	start    int64 // the box header's offset
	off, end int64 // the payload's range
	kids     []*node
	data     []byte // replacement payload; nil copies off..end from the source
}

var containers = map[string]bool{"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true}

// Inspect returns the fix r needs, or nil when every track's length header
// agrees with its sample table. Only box headers and a few small boxes are
// read unless a fix is needed.
func Inspect(r io.ReaderAt, size int64) (*Fix, error) {
	top, err := parse(r, 0, size, false)
	if err != nil {
		return nil, err
	}
	var moov *node
	for _, n := range top {
		if n.typ == "moov" {
			moov = n
		}
	}
	if moov == nil {
		return nil, nil
	}
	if moov.kids, err = parse(r, moov.off, moov.end, true); err != nil {
		return nil, err
	}

	mvhd := child(moov, "mvhd")
	if mvhd == nil {
		return nil, errMalformed
	}
	mvhdP, err := payload(r, mvhd)
	if err != nil {
		return nil, err
	}
	movieScale, movieDur, err := scaleAndDuration(mvhdP, 4)
	if err != nil {
		return nil, err
	}

	fixed := false
	newMovieDur := movieDur
	for _, trak := range moov.kids {
		if trak.typ != "trak" {
			continue
		}
		mdia := child(trak, "mdia")
		mdhd, tkhd := child(mdia, "mdhd"), child(trak, "tkhd")
		stts := child(child(child(mdia, "minf"), "stbl"), "stts")
		if mdhd == nil || tkhd == nil || stts == nil {
			continue
		}
		mdhdP, err := payload(r, mdhd)
		if err != nil {
			return nil, err
		}
		if len(mdhdP) < 1 || mdhdP[0] != 0 {
			continue // already 64-bit
		}
		mediaScale, mediaDur, err := scaleAndDuration(mdhdP, 4)
		if err != nil {
			return nil, err
		}
		sttsP, err := payload(r, stts)
		if err != nil {
			return nil, err
		}
		sum, err := sttsSum(sttsP)
		if err != nil {
			return nil, err
		}
		// Only the exact signature of a wrap: anything else is some other
		// disagreement this package doesn't claim to understand.
		if sum <= math.MaxUint32 || uint32(sum) != uint32(mediaDur) || mediaScale == 0 {
			continue
		}
		if mdhd.data, err = withDuration(mdhdP, 4, sum); err != nil {
			return nil, err
		}
		trackDur := sum * movieScale / mediaScale
		tkhdP, err := payload(r, tkhd)
		if err != nil {
			return nil, err
		}
		if tkhd.data, err = withDuration(tkhdP, 8, trackDur); err != nil {
			return nil, err
		}
		newMovieDur = max(newMovieDur, trackDur)
		fixed = true
	}
	if !fixed {
		return nil, nil
	}
	if newMovieDur != movieDur {
		if mvhd.data, err = withDuration(mvhdP, 4, newMovieDur); err != nil {
			return nil, err
		}
	}

	// Media data after the moov moves by however much the moov grew.
	f := &Fix{size: size, moovStart: moov.start, moovEnd: moov.end}
	delta := boxSize(moov) - (moov.end - moov.start)
	if delta != 0 {
		if err := shiftChunkOffsets(r, moov, moov.end, delta); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	buf.Grow(int(boxSize(moov)))
	if err := write(&buf, r, moov); err != nil {
		return nil, err
	}
	f.moov = buf.Bytes()
	return f, nil
}

type header struct {
	typ        string
	start, end int64
	hdr        int
}

func readHeader(r io.ReaderAt, off, limit int64) (header, error) {
	var b [16]byte
	if off+8 > limit {
		return header{}, errMalformed
	}
	if _, err := r.ReadAt(b[:8], off); err != nil {
		return header{}, err
	}
	size := int64(binary.BigEndian.Uint32(b[:4]))
	h := header{typ: string(b[4:8]), start: off, hdr: 8}
	switch size {
	case 1:
		if _, err := r.ReadAt(b[8:16], off+8); err != nil {
			return header{}, err
		}
		size, h.hdr = int64(binary.BigEndian.Uint64(b[8:16])), 16
	case 0:
		size = limit - off // to the end of the container
	}
	if size < int64(h.hdr) || off+size > limit {
		return header{}, errMalformed
	}
	h.end = off + size
	return h, nil
}

// parse lists the boxes in [start, end). With deep set it descends into the
// containers on the way to the boxes Inspect edits.
func parse(r io.ReaderAt, start, end int64, deep bool) ([]*node, error) {
	var out []*node
	for off := start; off+8 <= end; {
		h, err := readHeader(r, off, end)
		if err != nil {
			return nil, err
		}
		n := &node{typ: h.typ, start: h.start, off: h.start + int64(h.hdr), end: h.end}
		if deep && containers[h.typ] {
			if n.kids, err = parse(r, n.off, n.end, true); err != nil {
				return nil, err
			}
		}
		out = append(out, n)
		off = h.end
	}
	return out, nil
}

func child(n *node, typ string) *node {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.typ == typ {
			return k
		}
	}
	return nil
}

func payload(r io.ReaderAt, n *node) ([]byte, error) {
	if n.data != nil {
		return n.data, nil
	}
	p := make([]byte, n.end-n.off)
	if _, err := r.ReadAt(p, n.off); err != nil {
		return nil, err
	}
	return p, nil
}

// scaleAndDuration reads an mvhd or mdhd payload: mid is the size of the
// fields between the timestamps and the duration (the timescale).
func scaleAndDuration(p []byte, mid int) (scale, dur uint64, err error) {
	switch {
	case len(p) >= 4+8+mid+4 && p[0] == 0:
		return uint64(binary.BigEndian.Uint32(p[12:])), uint64(binary.BigEndian.Uint32(p[12+mid:])), nil
	case len(p) >= 4+16+mid+8 && p[0] == 1:
		return uint64(binary.BigEndian.Uint32(p[20:])), binary.BigEndian.Uint64(p[20+mid:]), nil
	}
	return 0, 0, errMalformed
}

// withDuration returns an mvhd, tkhd or mdhd payload carrying d, widened to
// version 1 when d doesn't fit 32 bits. mid is the size of the fields between
// the two timestamps and the duration: the timescale (4), or tkhd's track ID
// and reserved word (8).
func withDuration(p []byte, mid int, d uint64) ([]byte, error) {
	switch {
	case len(p) >= 4+16+mid+8 && p[0] == 1:
		out := bytes.Clone(p)
		binary.BigEndian.PutUint64(out[20+mid:], d)
		return out, nil
	case len(p) < 4+8+mid+4 || p[0] != 0:
		return nil, errMalformed
	case d <= math.MaxUint32:
		out := bytes.Clone(p)
		binary.BigEndian.PutUint32(out[12+mid:], uint32(d))
		return out, nil
	}
	out := make([]byte, 0, len(p)+12)
	out = append(out, 1, p[1], p[2], p[3])
	out = binary.BigEndian.AppendUint64(out, uint64(binary.BigEndian.Uint32(p[4:]))) // creation
	out = binary.BigEndian.AppendUint64(out, uint64(binary.BigEndian.Uint32(p[8:]))) // modification
	out = append(out, p[12:12+mid]...)
	out = binary.BigEndian.AppendUint64(out, d)
	return append(out, p[12+mid+4:]...), nil
}

func sttsSum(p []byte) (uint64, error) {
	if len(p) < 8 {
		return 0, errMalformed
	}
	n := int(binary.BigEndian.Uint32(p[4:]))
	if len(p) < 8+8*n {
		return 0, errMalformed
	}
	var sum uint64
	for i := range n {
		e := p[8+8*i:]
		sum += uint64(binary.BigEndian.Uint32(e)) * uint64(binary.BigEndian.Uint32(e[4:]))
	}
	return sum, nil
}

// shiftChunkOffsets moves every chunk offset at or past from by delta, in
// every track's stco or co64.
func shiftChunkOffsets(r io.ReaderAt, n *node, from, delta int64) error {
	for _, k := range n.kids {
		if err := shiftChunkOffsets(r, k, from, delta); err != nil {
			return err
		}
	}
	if n.typ != "stco" && n.typ != "co64" {
		return nil
	}
	p, err := payload(r, n)
	if err != nil {
		return err
	}
	if len(p) < 8 {
		return errMalformed
	}
	count := int(binary.BigEndian.Uint32(p[4:]))
	width := 4
	if n.typ == "co64" {
		width = 8
	}
	if len(p) < 8+width*count {
		return errMalformed
	}
	p = bytes.Clone(p)
	for i := range count {
		e := p[8+width*i:]
		if width == 8 {
			if v := int64(binary.BigEndian.Uint64(e)); v >= from {
				binary.BigEndian.PutUint64(e, uint64(v+delta))
			}
			continue
		}
		v := int64(binary.BigEndian.Uint32(e))
		if v < from {
			continue
		}
		if v+delta > math.MaxUint32 || v+delta < 0 {
			return fmt.Errorf("mp4fix: chunk offset %d no longer fits stco", v+delta)
		}
		binary.BigEndian.PutUint32(e, uint32(v+delta))
	}
	n.data = p
	return nil
}

func payloadSize(n *node) int64 {
	switch {
	case n.data != nil:
		return int64(len(n.data))
	case n.kids != nil:
		var s int64
		for _, k := range n.kids {
			s += boxSize(k)
		}
		return s
	}
	return n.end - n.off
}

func boxSize(n *node) int64 {
	p := payloadSize(n)
	if p+8 <= math.MaxUint32 {
		return p + 8
	}
	return p + 16
}

func write(w *bytes.Buffer, r io.ReaderAt, n *node) error {
	size := boxSize(n)
	if size-payloadSize(n) == 8 {
		w.Write(binary.BigEndian.AppendUint32(nil, uint32(size)))
		w.WriteString(n.typ)
	} else {
		w.Write(binary.BigEndian.AppendUint32(nil, 1))
		w.WriteString(n.typ)
		w.Write(binary.BigEndian.AppendUint64(nil, uint64(size)))
	}
	switch {
	case n.data != nil:
		w.Write(n.data)
	case n.kids != nil:
		for _, k := range n.kids {
			if err := write(w, r, k); err != nil {
				return err
			}
		}
	default:
		p, err := payload(r, n)
		if err != nil {
			return err
		}
		w.Write(p)
	}
	return nil
}

// reader is the corrected file: the original up to the moov, the rebuilt
// moov, then the original from the old moov's end on.
type reader struct {
	r   io.ReaderAt
	f   *Fix
	pos int64
}

func (x *reader) Read(p []byte) (int, error) {
	f := x.f
	if x.pos >= f.Size() {
		return 0, io.EOF
	}
	newMoovEnd := f.moovStart + int64(len(f.moov))
	var n int
	var err error
	switch {
	case x.pos < f.moovStart:
		n, err = x.r.ReadAt(clip(p, f.moovStart-x.pos), x.pos)
	case x.pos < newMoovEnd:
		n = copy(p, f.moov[x.pos-f.moovStart:])
	default:
		n, err = x.r.ReadAt(clip(p, f.Size()-x.pos), x.pos-f.delta())
	}
	x.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func clip(p []byte, n int64) []byte {
	if int64(len(p)) > n {
		return p[:n]
	}
	return p
}

func (x *reader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += x.pos
	case io.SeekEnd:
		offset += x.f.Size()
	}
	if offset < 0 {
		return 0, errors.New("mp4fix: negative position")
	}
	x.pos = offset
	return offset, nil
}
