package cut

import (
	"io"
	"testing"
)

// keyframePTS lists the PTS of each keyframe in a stream, in seconds.
func keyframePTS(b []byte) []float64 {
	var out []float64
	for off := 0; off+packetSize <= len(b); off += packetSize {
		pk, _ := parsePacket(b[off : off+packetSize])
		if pk.pid == testVideoPID && pk.rai {
			pts, _ := pesPTS(pk.payload)
			out = append(out, float64(pts)/90000)
		}
	}
	return out
}

// A reader that keeps up gets the stream as it was.
func TestLiveKeepsUp(t *testing.T) {
	in := testStream(10, 0)
	l := NewLive(3 * 90000)
	var out []byte
	buf := make([]byte, 1000)
	for off := 0; off < len(in); off += 500 { // writes that split packets
		l.Write(in[off:min(off+500, len(in))])
		for {
			n, _ := l.Read(buf)
			out = append(out, buf[:n]...)
			if len(l.chunks) <= 1 && len(l.chunks[0].data) == 0 {
				break
			}
		}
	}
	if string(out) != string(in[:len(in)/packetSize*packetSize]) || l.Dropped() != 0 {
		t.Errorf("got %d of %d bytes, dropped %d", len(out), len(in), l.Dropped())
	}
}

// A reader that falls 10 s behind is cut back to the newest keyframe, and the
// stream after the cut follows on in time.
func TestLiveCutsBacklog(t *testing.T) {
	l := NewLive(3 * 90000)
	cuts := 0
	l.OnCut = func(int64) { cuts++ }
	in := testStream(10, 90000)
	buf := make([]byte, 2*packetSize)
	l.Write(in[:2*packetSize]) // the PAT and PMT, read at once
	n, _ := l.Read(buf)
	out := append([]byte(nil), buf[:n]...)
	for off := 2 * packetSize; off < len(in); off += packetSize {
		l.Write(in[off : off+packetSize])
	}
	l.CloseWithError(nil)
	rest, err := io.ReadAll(l)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, rest...)
	got := keyframePTS(out)
	if cuts == 0 || len(got) < 2 || len(got) > 4 {
		t.Fatalf("%d cuts; keyframes at %v", cuts, got)
	}
	for i, pts := range got {
		if want := 1 + float64(i); pts < want-0.01 || pts > want+0.01 {
			t.Errorf("keyframe %d at %.2fs, want %.0fs (all: %v)", i, pts, want, got)
		}
	}
	if d := float64(l.Dropped()) / 90000; d < 6 || d > 9 {
		t.Errorf("dropped %.1fs", d)
	}
}
