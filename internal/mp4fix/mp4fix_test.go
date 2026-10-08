package mp4fix

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func box(typ string, parts ...[]byte) []byte {
	payload := bytes.Join(parts, nil)
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	return append(append(out, typ...), payload...)
}

func u32s(vs ...uint32) []byte {
	var out []byte
	for _, v := range vs {
		out = binary.BigEndian.AppendUint32(out, v)
	}
	return out
}

// book builds ftyp + moov + mdat with a 44.1 kHz track of the given AAC frame
// count, whose mdhd claims mdhdDur samples; the stco points at two chunks in
// mdat. Poseidon's Wake was 4,200,727 frames claiming the length mod 2^32.
func book(frames, mdhdDur uint32) []byte {
	ftyp := box("ftyp", []byte("M4A "), u32s(0), []byte("M4A isom"))
	moovFor := func(chunk0 uint32) []byte {
		mvhd := box("mvhd", u32s(0, 1, 2, 1000, 97_391_525), make([]byte, 80))
		tkhd := box("tkhd", u32s(0, 1, 2, 1, 0, 149_141), make([]byte, 60))
		mdhd := box("mdhd", u32s(0, 1, 2, 44100, mdhdDur), []byte{0x55, 0xc4, 0, 0})
		stbl := box("stbl",
			box("stts", u32s(0, 1, frames, 1024)),
			box("stsz", u32s(0, 0, 2, 4, 4)),
			box("stco", u32s(0, 2, chunk0, chunk0+4)))
		trak := box("trak", tkhd, box("mdia", mdhd, box("hdlr", make([]byte, 25)), box("minf", stbl)))
		return box("moov", mvhd, trak)
	}
	moovLen := len(moovFor(0))
	data := uint32(len(ftyp) + moovLen + 8) // mdat payload
	return bytes.Join([][]byte{ftyp, moovFor(data), box("mdat", []byte("AAAABBBB"))}, nil)
}

func find(t *testing.T, file []byte, path ...string) []byte {
	t.Helper()
	r := bytes.NewReader(file)
	nodes, err := parse(r, 0, int64(len(file)), true)
	if err != nil {
		t.Fatal(err)
	}
	var n *node
	for _, typ := range path {
		n = child(&node{kids: nodes}, typ)
		if n == nil {
			t.Fatalf("no %v", path)
		}
		nodes = n.kids
	}
	p, err := payload(r, n)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInspectRepairsWrappedTrack(t *testing.T) {
	const frames, wrap = 4_200_727, 1 << 32
	orig := book(frames, uint32(frames*1024%wrap))
	fix, err := Inspect(bytes.NewReader(orig), int64(len(orig)))
	if err != nil || fix == nil {
		t.Fatalf("fix = %v, %v", fix, err)
	}
	out, err := io.ReadAll(fix.Reader(bytes.NewReader(orig)))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(out)) != fix.Size() || len(out) != len(orig)+12 {
		t.Fatalf("size %d, Size() %d, original %d", len(out), fix.Size(), len(orig))
	}

	mdhd := find(t, out, "moov", "trak", "mdia", "mdhd")
	if mdhd[0] != 1 || binary.BigEndian.Uint64(mdhd[24:]) != frames*1024 || binary.BigEndian.Uint32(mdhd[20:]) != 44100 {
		t.Errorf("mdhd not widened to the full length: % x", mdhd)
	}
	if lang := mdhd[32:34]; lang[0] != 0x55 || lang[1] != 0xc4 {
		t.Errorf("language lost: % x", lang)
	}
	// 4,301,544,448 samples at 44.1 kHz = 97,540,690 ms, which fits 32 bits.
	if tk := find(t, out, "moov", "trak", "tkhd"); tk[0] != 0 || binary.BigEndian.Uint32(tk[20:]) != 97_540_690 {
		t.Errorf("tkhd duration %d", binary.BigEndian.Uint32(tk[20:]))
	}
	if mv := find(t, out, "moov", "mvhd"); binary.BigEndian.Uint32(mv[16:]) != 97_540_690 {
		t.Errorf("mvhd duration %d", binary.BigEndian.Uint32(mv[16:]))
	}
	// Chunk offsets follow the moov's growth and still point at the data.
	stco := find(t, out, "moov", "trak", "mdia", "minf", "stbl", "stco")
	for i, want := range []string{"AAAA", "BBBB"} {
		off := binary.BigEndian.Uint32(stco[8+4*i:])
		if got := string(out[off : off+4]); got != want {
			t.Errorf("chunk %d at %d reads %q, want %q", i, off, got, want)
		}
	}
}

func TestInspectLeavesHealthyFilesAlone(t *testing.T) {
	// The header agrees with the sample table.
	healthy := book(6423, 6423*1024)
	if fix, err := Inspect(bytes.NewReader(healthy), int64(len(healthy))); err != nil || fix != nil {
		t.Errorf("fix = %v, %v; want nothing to do", fix, err)
	}
	// Not an MP4 at all: no moov, nothing to do.
	mp3 := []byte("ID3\x04\x00\x00\x00\x00\x00\x00 not an mp4")
	if fix, _ := Inspect(bytes.NewReader(mp3), int64(len(mp3))); fix != nil {
		t.Error("non-MP4 input produced a fix")
	}
}

func TestReaderSeeks(t *testing.T) {
	orig := book(4_200_727, 4_200_727*1024%(1<<32))
	fix, err := Inspect(bytes.NewReader(orig), int64(len(orig)))
	if err != nil || fix == nil {
		t.Fatal(fix, err)
	}
	full, _ := io.ReadAll(fix.Reader(bytes.NewReader(orig)))
	for _, at := range []int64{0, 5, fix.moovStart + 3, fix.moovStart + int64(len(fix.moov)) - 2, fix.Size() - 3} {
		rs := fix.Reader(bytes.NewReader(orig))
		if _, err := rs.Seek(at, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rs)
		if !bytes.Equal(got, full[at:]) {
			t.Errorf("read from %d differs", at)
		}
	}
	if end, _ := fix.Reader(bytes.NewReader(orig)).Seek(0, io.SeekEnd); end != fix.Size() {
		t.Errorf("SeekEnd = %d, want %d", end, fix.Size())
	}
}
