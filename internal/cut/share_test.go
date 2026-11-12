package cut

import (
	"io"
	"testing"
)

// readAll reads what r has until it waits for more.
func readAll(r *ShareReader) []byte {
	var out []byte
	buf := make([]byte, 1000)
	for {
		r.mu.Lock()
		empty := len(r.buf) == 0
		r.mu.Unlock()
		if empty {
			return out
		}
		n, _ := r.Read(buf)
		out = append(out, buf[:n]...)
	}
}

// A reader that joins partway starts at the newest keyframe, after the PAT
// and PMT.
func TestShareJoinsAtKeyframe(t *testing.T) {
	in := testStream(10, 0)
	s := NewShare()
	half := len(in) / 2 / packetSize * packetSize
	s.Write(in[:half+100]) // a packet split across writes
	r := s.Reader(1<<30, nil)
	s.Write(in[half+100:])
	out := readAll(r)
	pk, _ := parsePacket(out)
	if pk.pid != 0 {
		t.Errorf("starts on PID %d, not the PAT", pk.pid)
	}
	pk, _ = parsePacket(out[2*packetSize:])
	if pk.pid != testVideoPID || !pk.rai {
		t.Errorf("third packet is PID %d, keyframe %v", pk.pid, pk.rai)
	}
	got := keyframePTS(out)
	if len(got) != 6 || got[0] != 4 || got[5] != 9 {
		t.Errorf("keyframes at %v, want 4 to 9", got)
	}
	select {
	case <-s.Ready():
	default:
		t.Error("not ready")
	}
}

// A reader that falls behind skips to the next keyframe on its own, and the
// stream after the skip follows on in time; another reader is not held back.
func TestShareSlowReaderSkips(t *testing.T) {
	in := testStream(10, 0)
	s := NewShare()
	skipped := 0
	slow := s.Reader(3*len(in)/10, func() { skipped++ })
	fast := s.Reader(1<<30, nil)
	sec := len(in) / 10
	var out []byte
	for i := 0; i < 10; i++ {
		end := (i + 1) * sec
		if i == 9 {
			end = len(in)
		}
		s.Write(in[i*sec : end])
		if i == 0 || i >= 4 { // takes nothing from 1 s to 4 s
			out = append(out, readAll(slow)...)
		}
	}
	s.CloseWithError(nil)
	rest, err := io.ReadAll(slow)
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, rest...)
	got := keyframePTS(out)
	if skipped != 1 || slow.Skips() != 1 || len(got) < 6 {
		t.Fatalf("%d skips; keyframes at %v", skipped, got)
	}
	for i := 1; i < len(got); i++ {
		if d := got[i] - got[i-1]; d < 0.5 || d > 1.01 {
			t.Errorf("keyframe %d %.2fs after the one before (all: %v)", i, d, got)
		}
	}
	if all, _ := io.ReadAll(fast); len(all) != len(in)/packetSize*packetSize {
		t.Errorf("the other reader got %d of %d bytes", len(all), len(in))
	}
}

// A stream of sound alone starts at a PES of the sound.
func TestShareSoundAlone(t *testing.T) {
	pat := []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe0 | 0x10>>8, 0x10, 0, 0, 0, 0}
	pmt := []byte{0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x01, 0xf0, 0, 0x0f, 0xe1, 0x01, 0xf0, 0, 0, 0, 0, 0}
	s := NewShare()
	s.Write(tsPacket(0, true, false, section(pat)))
	s.Write(tsPacket(0x10, true, false, section(pmt)))
	s.Write(tsPacket(0x101, false, false, []byte{1, 2, 3})) // the end of a PES
	r := s.Reader(1<<30, nil)
	s.Write(tsPacket(0x101, true, false, []byte{0, 0, 1, 0xc0, 0, 0, 0x80, 0, 0}))
	out := readAll(r)
	if len(out) != 3*packetSize {
		t.Fatalf("got %d packets", len(out)/packetSize)
	}
	if pk, _ := parsePacket(out[2*packetSize:]); pk.pid != 0x101 || !pk.pusi {
		t.Errorf("starts at PID %d", pk.pid)
	}
}

// Keyframes that are not flagged are known by the start of their picture,
// even when it runs past the PES's first packet.
func TestShareFindsUnflaggedKeyframes(t *testing.T) {
	in := testStreamNAL(10, 0, true)
	s := NewShare()
	half := len(in) / 2 / packetSize * packetSize
	s.Write(in[:half])
	r := s.Reader(1<<30, nil)
	s.Write(in[half:])
	s.CloseWithError(nil)
	out, _ := io.ReadAll(r)
	pk, _ := parsePacket(out[2*packetSize:])
	pts, _ := pesPTS(pk.payload)
	if pk.pid != testVideoPID || !pk.pusi || pts != 4*90000 {
		t.Errorf("starts at PID %d, PTS %d", pk.pid, pts)
	}
	if want := len(in) - half + (half - 4*len(in)/10/packetSize*packetSize); len(out) < len(in)-half {
		t.Errorf("got %d bytes, want about %d", len(out), want)
	}
}
