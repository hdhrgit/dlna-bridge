package cut

import (
	"bytes"
	"context"
	"io"
	"math"
	"testing"
)

type memSource []byte

func (m memSource) Size() int64 { return int64(len(m)) }

func (m memSource) ReadAt(_ context.Context, off, n int64) ([]byte, error) {
	if off >= int64(len(m)) {
		return nil, io.EOF
	}
	return m[off:min(off+n, int64(len(m)))], nil
}

func (m memSource) open(_ context.Context, off, n int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m[off : off+n])), nil
}

const (
	testVideoPID = 0x100
	testFPS      = 30
	testGOP      = 30 // a keyframe every second
)

func tsPacket(pid int, pusi, rai bool, payload []byte) []byte {
	p := make([]byte, packetSize)
	p[0] = 0x47
	p[1] = byte(pid >> 8 & 0x1f)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	i := 4
	if rai {
		p[3] = 0x30 // adaptation field and payload
		p[4], p[5] = 1, 0x40
		i = 6
	} else {
		p[3] = 0x10
	}
	n := copy(p[i:], payload)
	for j := i + n; j < packetSize; j++ {
		p[j] = 0xff
	}
	return p
}

func section(b []byte) []byte { return append([]byte{0}, b...) } // pointer field

// testStream is secs seconds of video starting at PTS base, 21 packets a frame.
func testStream(secs int, base int64) []byte {
	var out []byte
	pat := []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe0 | 0x10>>8, 0x10, 0, 0, 0, 0}
	pmt := []byte{0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, 0x1b, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0}
	out = append(out, tsPacket(0, true, false, section(pat))...)
	out = append(out, tsPacket(0x10, true, false, section(pmt))...)
	for f := 0; f < secs*testFPS; f++ {
		pts := (base + int64(f)*90000/testFPS) % ptsWrap
		pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5,
			byte(0x21 | pts>>29&0x0e), byte(pts >> 22), byte(pts>>14 | 1), byte(pts >> 7), byte(pts<<1 | 1),
			0, 0, 1, 0x41} // a non-IDR slice
		out = append(out, tsPacket(testVideoPID, true, f%testGOP == 0, pes)...)
		for range 20 {
			out = append(out, tsPacket(testVideoPID, false, false, []byte{1, 2, 3})...)
		}
	}
	return out
}

// timeAt is the video time of the frame whose PES starts at off.
func timeAt(t *testing.T, b []byte, off int64, base int64) float64 {
	pk, ok := parsePacket(b[off:])
	if !ok || !pk.pusi {
		t.Fatalf("offset %d is not the start of a frame", off)
	}
	pts, _ := pesPTS(pk.payload)
	return float64((pts-base+ptsWrap)%ptsWrap) / 90000
}

func TestMake(t *testing.T) {
	// A base near the wrap checks that times keep counting up through it.
	base := int64(ptsWrap - 5*90000)
	src := memSource(testStream(60, base))
	p, err := Make(context.Background(), src, []float64{10.2, 20.5, 40, 45})
	if err != nil {
		t.Fatal(err)
	}
	var kept []Part
	for _, part := range p.Parts {
		if part.Data == nil {
			kept = append(kept, part)
		}
	}
	if len(kept) != 3 || kept[0].Off != 0 {
		t.Fatalf("parts: %+v", p.Parts)
	}
	// Each break begins within a few frames of its mark...
	if end := timeAt(t, src, kept[0].Off+kept[0].Len, base); math.Abs(end-10.2) > 0.5 {
		t.Errorf("first break starts at %.2fs", end)
	}
	// ...and the recording picks up on the first keyframe after it ends.
	if got := timeAt(t, src, kept[1].Off, base); got != 21 {
		t.Errorf("after the first break: %.2fs, want the keyframe at 21s", got)
	}
	if got := timeAt(t, src, kept[2].Off, base); got != 45 {
		t.Errorf("after the second break: %.2fs, want 45s", got)
	}
	if kept[2].Off+kept[2].Len != src.Size() {
		t.Errorf("last part ends at %d of %d", kept[2].Off+kept[2].Len, src.Size())
	}
	if got := p.CutTime(30); math.Abs(got-(10.2+9)) > 0.01 {
		t.Errorf("CutTime(30) = %.2f", got)
	}

	// Reading the file gives the parts in order, with PAT and PMT before each
	// part after a break; seeking lands on the same bytes.
	f := p.Open(context.Background(), src.open)
	all, err := io.ReadAll(f)
	if err != nil || int64(len(all)) != p.Size {
		t.Fatalf("read %d of %d: %v", len(all), p.Size, err)
	}
	var want []byte
	for _, part := range p.Parts {
		if part.Data != nil {
			want = append(want, part.Data...)
		} else {
			want = append(want, src[part.Off:part.Off+part.Len]...)
		}
	}
	if !bytes.Equal(all, want) {
		t.Fatal("bytes differ from the parts")
	}
	if sync(all) != 0 || len(all)%packetSize != 0 {
		t.Error("the cut file is not whole packets")
	}
	off := kept[0].Len + 100
	f.Seek(off, io.SeekStart)
	b := make([]byte, 1000)
	n, _ := io.ReadFull(f, b)
	if !bytes.Equal(b[:n], all[off:off+int64(n)]) {
		t.Error("seek read different bytes")
	}
}

func TestBreakToTheEnd(t *testing.T) {
	src := memSource(testStream(20, 1000))
	p, err := Make(context.Background(), src, []float64{15, 25})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Parts) != 1 || p.Parts[0].Off != 0 {
		t.Fatalf("parts: %+v", p.Parts)
	}
}
