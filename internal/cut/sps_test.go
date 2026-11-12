package cut

import (
	"encoding/hex"
	"testing"
)

// SPSs from x264 as the bridge's ffmpeg runs it, and from a few sources.
var testSPS = []struct {
	hex  string
	w, h int
	fps  float64
}{
	{"6742c015d901c087ef011000000300100000030308f162e48000", 448, 252, 24},                   // baseline, one slice
	{"6742c00cd90342febc0440000003004000000783c50a9200", 206, 368, 15},                       // cropped on both sides
	{"67640028acd94078044fde022000007d20001d4c03e2c5b2c000", 1920, 1080, 30000.0 / 1001},     // high, interlaced
	{"6764001eacd940b43dbff0020001b100000303e90000ea600f162d9600", 720, 480, 30000.0 / 1001}, // high, SAR 32:27
}

func TestParseSPS(t *testing.T) {
	for _, c := range testSPS {
		b, _ := hex.DecodeString(c.hex)
		w, h, fps, err := parseSPS(b[1:])
		if err != nil || w != c.w || h != c.h || fps != c.fps {
			t.Errorf("%s: %dx%d %v %v, want %dx%d %v", c.hex, w, h, fps, err, c.w, c.h, c.fps)
		}
	}
	b, _ := hex.DecodeString(testSPS[0].hex)
	if _, _, _, err := parseSPS(b[1:8]); err == nil {
		t.Error("cut short: no error")
	}
}

// The format is read from an SPS that runs on into the stream's next packet,
// once that packet is there.
func TestVideoFormat(t *testing.T) {
	sps, _ := hex.DecodeString(testSPS[0].hex)
	pat := []byte{0x00, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xe0 | 0x10>>8, 0x10, 0, 0, 0, 0}
	pmt := []byte{0x02, 0xb0, 18, 0, 1, 0xc1, 0, 0, 0xe1, 0x00, 0xf0, 0, 0x1b, 0xe1, 0x00, 0xf0, 0, 0, 0, 0, 0}
	// A picture delimiter, filler to the end of the packet but for the SPS's
	// first 10 bytes, which tsPacket would otherwise follow with its own.
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0, 0, 0, 0, 1, 0x09, 0xf0, 0, 0, 1, 0x0c}
	for len(pes) < packetSize-6-14 {
		pes = append(pes, 0xff)
	}
	pes = append(append(pes, 0x80, 0, 0, 1), sps[:10]...)
	var b []byte
	b = append(b, tsPacket(0, true, false, section(pat))...)
	b = append(b, tsPacket(0x10, true, false, section(pmt))...)
	b = append(b, tsPacket(testVideoPID, true, true, pes)...)
	// An audio packet between, then the rest of the SPS and the PPS's start.
	b = append(b, tsPacket(0x101, true, false, []byte{0, 0, 1, 0xc0})...)
	if _, _, _, ok := VideoFormat(b); ok {
		t.Fatal("half an SPS read")
	}
	b = append(b, tsPacket(testVideoPID, false, false, append(append([]byte(nil), sps[10:]...), 0, 0, 1, 0x68))...)
	if w, h, fps, ok := VideoFormat(b); !ok || w != 448 || h != 252 || fps != 24 {
		t.Errorf("got %dx%d %v %v", w, h, fps, ok)
	}
	if _, _, _, ok := VideoFormat(b[:3*packetSize-1]); ok {
		t.Error("read from a partial packet")
	}
}
