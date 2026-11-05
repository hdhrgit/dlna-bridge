package cut

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// Frames come in decode order I B B P P...: the two B-frames after each
// keyframe are shown before it, as in an open GOP.
func testStream(secs int, base int64) []byte { return testStreamNAL(secs, base, false) }

// testStreamNAL is testStream, and with lateNAL the keyframes are not flagged
// in the adaptation field and show their IDR slice only in the second packet of
// the PES, after a picture delimiter in the first, as many streams do.
func testStreamNAL(secs int, base int64, lateNAL bool) []byte {
	var out []byte
	pat := []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe0 | 0x10>>8, 0x10, 0, 0, 0, 0}
	pmt := []byte{0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, 0x1b, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0}
	out = append(out, tsPacket(0, true, false, section(pat))...)
	out = append(out, tsPacket(0x10, true, false, section(pmt))...)
	for f := 0; f < secs*testFPS; f++ {
		e := int64(f) // the frame's place in display order
		switch k := f % testGOP; {
		case k == 0:
		case k < 3:
			e -= 3
		default:
			e -= 2
		}
		pts := (base + e*90000/testFPS + ptsWrap) % ptsWrap
		pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5,
			byte(0x21 | pts>>29&0x0e), byte(pts >> 22), byte(pts>>14 | 1), byte(pts >> 7), byte(pts<<1 | 1),
			0, 0, 1, 0x41} // a non-IDR slice
		key := f%testGOP == 0
		more := []byte{1, 2, 3}
		if lateNAL && key {
			pes = append(pes[:14:14], 0, 0, 1, 0x09, 0xf0) // a picture delimiter, not a slice
			more = []byte{0, 0, 1, 0x65, 8, 8}             // the IDR slice
		}
		out = append(out, tsPacket(testVideoPID, true, key && !lateNAL, pes)...)
		for i := 0; i < 20; i++ {
			payload := []byte{1, 2, 3}
			if i == 0 {
				payload = more
			}
			out = append(out, tsPacket(testVideoPID, false, false, payload)...)
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
	if sync(all) != 0 || len(all)%packetSize != 0 {
		t.Error("the cut file is not whole packets")
	}
	// The timestamps run on across the joins, and no keyframe after one is
	// followed by a picture shown before it.
	var last int64 = -1
	var key int64
	var lead, joins, frames int
	var afterPAT bool
	for i := 0; i+packetSize <= len(all); i += packetSize {
		pk, _ := parsePacket(all[i:])
		if pk.pid == 0 && i > 0 { // the PAT that opens each part after a break
			afterPAT = true
		}
		if pk.pid != testVideoPID || !pk.pusi {
			continue
		}
		pts, _ := pesPTS(pk.payload)
		if last >= 0 && math.Abs(float64((pts-last+3*ptsWrap/2)%ptsWrap-ptsWrap/2)) > 90000 {
			t.Fatalf("frame %d: PTS %d after %d", frames, pts, last)
		}
		if pk.rai {
			key, lead = pts, 0
			if afterPAT {
				lead = 3
				joins++
			}
			afterPAT = false
		} else if lead > 0 {
			lead--
			if (pts-key+3*ptsWrap/2)%ptsWrap-ptsWrap/2 < 0 {
				t.Errorf("frame %d, %d after the keyframe at a join: PTS %d is before its %d", frames, 3-lead, pts, key)
			}
		}
		last = pts
		frames++
	}
	if joins != 2 {
		t.Errorf("%d joins", joins)
	}
	// Seeking, even into the middle of a packet, reads the same bytes.
	for _, off := range []int64{kept[0].Len + 100, kept[0].Len + packetSize*2 + 7, p.Size - 500} {
		f.Seek(off, io.SeekStart)
		b := make([]byte, 1000)
		n, _ := io.ReadFull(f, b)
		if n == 0 || !bytes.Equal(b[:n], all[off:off+int64(n)]) {
			t.Errorf("seek to %d read different bytes", off)
		}
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

// A PES header that says it is longer than the packet that holds it, as some
// streams have, is not read past the end.
func TestKeyframeNALShortPacket(t *testing.T) {
	pes := make([]byte, 184)
	pes[8] = 200 // header length beyond the packet
	if keyframeStart(pes, 0x1b) || keyframeStart(pes, 0x24) || keyframeStart(pes, 0x02) {
		t.Error("found a keyframe in a header that runs past the packet")
	}
}

// Keyframes whose slice comes after the first packet are found without the
// keyframe flag.
func TestKeyframeAfterTheFirstPacket(t *testing.T) {
	src := memSource(testStreamNAL(60, 90000, true))
	p, err := Make(context.Background(), src, []float64{10.2, 20.5})
	if err != nil {
		t.Fatal(err)
	}
	var kept []Part
	for _, part := range p.Parts {
		if part.Data == nil {
			kept = append(kept, part)
		}
	}
	if len(kept) != 2 {
		t.Fatalf("parts: %+v", p.Parts)
	}
	// The second part starts on the keyframe at 21 s, though nothing flags it.
	if got := timeAt(t, src, kept[1].Off, 90000); got < 20.5 || got > 22.1 {
		t.Errorf("resumes at %.2fs, want the keyframe at about 21s", got)
	}
}

// A recording whose timestamps jump does not keep time with the DVR's marks,
// and is refused rather than cut in the wrong places.
func TestTimestampJumpIsRefused(t *testing.T) {
	a, b := testStream(40, 90000), testStream(40, 90000+5000*90000)
	_, err := Make(context.Background(), memSource(append(a, b...)), []float64{50, 55})
	if !errors.Is(err, ErrTimeline) {
		t.Errorf("err = %v, want the timeline error", err)
	}
}

// countingSource counts what is asked of the recording.
type countingSource struct {
	memSource
	reads int
	bytes int64
}

func (c *countingSource) ReadAt(ctx context.Context, off, n int64) ([]byte, error) {
	b, err := c.memSource.ReadAt(ctx, off, n)
	c.reads++
	c.bytes += int64(len(b))
	return b, err
}

// Six breaks in ten minutes are found in few reads, at the right frames.
func TestFewReads(t *testing.T) {
	const secs = 600
	base := int64(90000)
	data := testStream(secs, base)
	src := &countingSource{memSource: memSource(data)}
	breaks := []float64{0, 12.3, 80.7, 131.2, 200.05, 241.9, 300.4, 352.8, 420.0, 470.5, 540.2, 590.0}
	p, err := Make(context.Background(), src, breaks)
	if err != nil {
		t.Fatal(err)
	}
	var kept []Part
	for _, part := range p.Parts {
		if part.Data == nil {
			kept = append(kept, part)
		}
	}
	// A break that starts at 0 leaves only the few packets before the first
	// picture (the tables) ahead of it.
	if len(kept) > 0 && kept[0].Off == 0 && kept[0].Len < 4096 {
		kept = kept[1:]
	}
	// The first break starts at 0, so each kept part starts after break i (on the
	// first keyframe past its end) and ends where break i+1 starts.
	if len(kept) != len(breaks)/2 {
		t.Fatalf("%d kept parts, want %d", len(kept), len(breaks)/2)
	}
	for i, part := range kept {
		startT := timeAt(t, memSource(data), part.Off, base)
		if to := breaks[2*i+1]; startT < to || startT > to+1.1 {
			t.Errorf("part %d starts at %.2fs, its break ends at %.2fs", i, startT, to)
		}
		if i+1 < len(kept) {
			endT := timeAt(t, memSource(data), part.Off+part.Len, base)
			if from := breaks[2*i+2]; endT < from || endT > from+0.5 {
				t.Errorf("part %d ends at %.2fs, the next break starts at %.2fs", i, endT, from)
			}
		}
	}
	// Halving the file for each of 12 marks took about 12 x 8 reads.
	if src.reads > 60 {
		t.Errorf("%d reads for %d breaks", src.reads, len(breaks)/2)
	}
	t.Logf("%d reads, %d kB of %d kB", src.reads, src.bytes>>10, len(data)>>10)
}

// The block cache gives the same bytes as the recording, and asks for a run of
// missing blocks once.
func TestBlockCache(t *testing.T) {
	data := make([]byte, 700<<10+123)
	for i := range data {
		data[i] = byte(i * 7)
	}
	src := &countingSource{memSource: memSource(data)}
	c := newBlockCache(src)
	ctx := context.Background()
	for _, r := range [][2]int64{{0, 100}, {5000, 200 << 10}, {100 << 10, 50 << 10}, {int64(len(data)) - 10, 100}, {300 << 10, 1}, {0, 700<<10 + 123}} {
		got, err := c.ReadAt(ctx, r[0], r[1])
		want := data[r[0]:min(r[0]+r[1], int64(len(data)))]
		if err != nil || string(got) != string(want) {
			t.Fatalf("read %v: %d bytes (%v), want %d", r, len(got), err, len(want))
		}
	}
	reads := src.reads
	if _, err := c.ReadAt(ctx, 150<<10, 10<<10); err != nil || src.reads != reads {
		t.Errorf("a read inside what is held asked the recording again (%d reads, was %d)", src.reads, reads)
	}
	if _, err := c.ReadAt(ctx, int64(len(data)), 10); err == nil {
		t.Error("a read past the end succeeded")
	}
	if len(c.blocks) > maxBlocks {
		t.Errorf("%d blocks held", len(c.blocks))
	}
}

// A plan survives a trip through JSON, and one that does not fit the recording
// is not valid.
func TestPlanJSON(t *testing.T) {
	src := memSource(testStream(60, 90000))
	p, err := Make(context.Background(), src, []float64{10.2, 20.5, 40, 45})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var q Plan
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	if !q.Valid(src.Size()) || q.Size != p.Size || len(q.Parts) != len(p.Parts) || q.videoPID != p.videoPID {
		t.Errorf("after JSON: %+v", q)
	}
	// Reading it gives the same bytes as the original.
	a, _ := io.ReadAll(p.Open(context.Background(), src.open))
	c, _ := io.ReadAll(q.Open(context.Background(), src.open))
	if !bytes.Equal(a, c) {
		t.Error("the restored plan reads differently")
	}
	if q.Valid(src.Size() / 2) {
		t.Error("a plan for a longer file is valid for a shorter one")
	}
	q.Size++
	if q.Valid(src.Size()) {
		t.Error("a plan with the wrong size is valid")
	}
}

func TestResync(t *testing.T) {
	pk := func() []byte { return tsPacket(testVideoPID, false, false, []byte{1}) }
	var b []byte
	b = append(b, pk()...)
	b = append(b, pk()[:100]...) // cut short
	b = append(b, pk()...)
	b = append(b, pk()...)
	b = append(b, pk()...)
	if got := resync(b, 0); got != 0 {
		t.Errorf("at a whole packet: %d", got)
	}
	if got := resync(b, packetSize); got != packetSize+100 {
		t.Errorf("after the short packet: %d, want %d", got, packetSize+100)
	}
	if got := resync(b, 7); got != packetSize+100 {
		t.Errorf("from inside a packet: %d", got)
	}
}

// damage cuts a packet short every so often from the byte at from, as a weak
// signal does, so the packets after it are off the grid.
func damage(b []byte, from int64, every int) []byte {
	var out []byte
	out = append(out, b[:from]...)
	for i, n := from, 0; i+packetSize <= int64(len(b)); i, n = i+packetSize, n+1 {
		p := b[i : i+packetSize]
		pk, _ := parsePacket(p)
		if n%every == every-1 && !pk.pusi && pk.pid == testVideoPID {
			out = append(out, p[:60+n%100]...)
			continue
		}
		out = append(out, p...)
	}
	return out
}

// A recording with damaged packets throughout is still cut: the breaks are
// found, the timestamps after them run on, and seeking reads the same bytes.
func TestDamagedRecording(t *testing.T) {
	base := int64(90000)
	src := memSource(damage(testStream(60, base), 50*packetSize, 97))
	p, err := Make(context.Background(), src, []float64{10.2, 20.5, 40, 45})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Valid(src.Size()) {
		t.Error("the plan is not valid for the file")
	}
	f := p.Open(context.Background(), src.open)
	all, err := io.ReadAll(f)
	if err != nil || int64(len(all)) != p.Size {
		t.Fatalf("read %d of %d: %v", len(all), p.Size, err)
	}
	// Every whole video frame start keeps time within a second of the one
	// before, across the joins.
	var last int64 = -1
	frames := 0
	for i := resync(all, 0); i >= 0; i = resync(all, i+packetSize) {
		pk, _ := parsePacket(all[i:])
		if pk.pid != testVideoPID || !pk.pusi {
			continue
		}
		pts, ok := pesPTS(pk.payload)
		if !ok {
			continue
		}
		if last >= 0 && math.Abs(float64(pts-last)) > 90000 {
			t.Fatalf("frame %d: PTS %d after %d", frames, pts, last)
		}
		last = pts
		frames++
	}
	// 60 s at 30 a second, less about 15 s of breaks.
	if frames < 40*testFPS || frames > 47*testFPS {
		t.Errorf("%d frames", frames)
	}
	for _, off := range []int64{100, p.Size / 3, p.Size/3 + 7, p.Size / 2, p.Size - 500} {
		f.Seek(off, io.SeekStart)
		b := make([]byte, 1000)
		n, _ := io.ReadFull(f, b)
		if n == 0 || !bytes.Equal(b[:n], all[off:off+int64(n)]) {
			t.Errorf("seek to %d read different bytes", off)
		}
	}
}

// MPEG-2 has no NAL units. Its slice start codes, read as H.264, look like
// IDRs (0x05) and SPSs (0x07); only an I picture is a keyframe.
func TestKeyframeMPEG2(t *testing.T) {
	pes := func(pictureType byte) []byte {
		b := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1}
		b = append(b, 0, 0, 1, 0x00, 0x00, pictureType<<3)   // picture start: type
		b = append(b, 0, 0, 1, 0x05, 9, 9, 0, 0, 1, 0x07, 9) // slices 5 and 7
		return b
	}
	if !keyframeStart(pes(1), 0x02) {
		t.Error("an I picture is not taken for a keyframe")
	}
	if keyframeStart(pes(2), 0x02) || keyframeStart(pes(3), 0x02) {
		t.Error("a P or B picture is taken for a keyframe")
	}
	if !keyframeStart(pes(2), 0x1b) {
		t.Error("the same bytes as H.264 should read as a keyframe (it shows why the codec matters)")
	}
}

// A frame stamped far off, as damage leaves, is not taken for the frame
// after a break: the cuts land where they would without it.
func TestStrayTimestamp(t *testing.T) {
	base := int64(90000)
	clean := testStream(60, base)
	bad := append([]byte(nil), clean...)
	// Restamp the first frame shown at or after 20.5 s, where the first break
	// ends, as 6 hours on.
	for i := 0; i+packetSize <= len(bad); i += packetSize {
		pk, _ := parsePacket(bad[i:])
		if pk.pid != testVideoPID || !pk.pusi {
			continue
		}
		if pts, ok := pesPTS(pk.payload); ok && pts-base >= 20.5*90000 {
			p := pk.payload[9:14]
			v := base + 6*3600*90000
			p[0], p[1], p[2], p[3], p[4] = byte(0x21|v>>29&0x0e), byte(v>>22), byte(v>>14|1), byte(v>>7), byte(v<<1|1)
			break
		}
	}
	breaks := []float64{10.2, 20.5, 40, 45}
	want, err := Make(context.Background(), memSource(clean), breaks)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Make(context.Background(), memSource(bad), breaks)
	if err != nil {
		t.Fatalf("with a stray timestamp: %v", err)
	}
	if fmt.Sprint(got.Parts[2].Off, got.Kept) != fmt.Sprint(want.Parts[2].Off, want.Kept) {
		t.Errorf("cuts moved: %v %v, want %v %v", got.Parts[2].Off, got.Kept, want.Parts[2].Off, want.Kept)
	}
}
