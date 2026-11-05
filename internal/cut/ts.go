// Package cut serves a recording with its commercial breaks left out, as one
// seekable file made of the byte ranges between the breaks. Nothing is
// re-encoded: each part after a break starts on a keyframe, with the stream's
// PAT and PMT in front so a player can pick it up there.
package cut

import (
	"errors"
)

const packetSize = 188

// Timestamps are 33-bit, at 90 kHz.
const ptsWrap = 1 << 33

// streams is what the PAT and PMT say about a transport stream.
type streams struct {
	pmtPID   int
	videoPID int
	codec    byte   // the video's stream type: 0x01/0x02 MPEG-1/2, 0x1b H.264, 0x24 HEVC
	pat, pmt []byte // the raw packets, to repeat in front of each part
}

// sync finds the first offset in b where packets line up on 0x47.
func sync(b []byte) int {
	for i := 0; i+2*packetSize < len(b) && i < packetSize; i++ {
		if b[i] == 0x47 && b[i+packetSize] == 0x47 && b[i+2*packetSize] == 0x47 {
			return i
		}
	}
	return -1
}

// resync is the start of the first whole packet at or after i in b, or -1. A
// recording can hold damaged packets, cut short where the signal dropped, after
// which the packets no longer fall every packetSize bytes from the start; past
// one, the next start is where three packets line up again. A packet is taken
// as whole when the next one starts right after it, or b ends first.
func resync(b []byte, i int) int {
	if i+packetSize > len(b) {
		return -1
	}
	if b[i] == 0x47 && (i+2*packetSize > len(b) || b[i+packetSize] == 0x47) {
		return i
	}
	for j := i + 1; j+packetSize <= len(b); j++ {
		if b[j] != 0x47 {
			continue
		}
		if j+3*packetSize > len(b) {
			return -1 // too near the end to tell: the next read will
		}
		if b[j+packetSize] == 0x47 && b[j+2*packetSize] == 0x47 {
			return j
		}
	}
	return -1
}

type packet struct {
	pid     int
	pusi    bool // a PES or section starts here
	rai     bool // random access indicator: a keyframe starts here
	payload []byte
}

func parsePacket(p []byte) (packet, bool) {
	if len(p) < packetSize || p[0] != 0x47 {
		return packet{}, false
	}
	pk := packet{pid: int(p[1]&0x1f)<<8 | int(p[2]), pusi: p[1]&0x40 != 0}
	afc := p[3] >> 4 & 3
	i := 4
	if afc&2 != 0 { // adaptation field
		n := int(p[4])
		if n > 0 {
			pk.rai = p[5]&0x40 != 0
		}
		i = 5 + n
	}
	if afc&1 != 0 && i < packetSize {
		pk.payload = p[i:packetSize]
	}
	return pk, true
}

// findStreams reads the PAT and PMT near the start of the file.
func findStreams(b []byte) (streams, error) {
	s := streams{pmtPID: -1, videoPID: -1}
	start := sync(b)
	if start < 0 {
		return s, errors.New("not an MPEG transport stream")
	}
	for off := start; off+packetSize <= len(b); off += packetSize {
		raw := b[off : off+packetSize]
		pk, ok := parsePacket(raw)
		if !ok || !pk.pusi || len(pk.payload) == 0 || 1+int(pk.payload[0]) >= len(pk.payload) {
			continue
		}
		sec := pk.payload[1+int(pk.payload[0]):] // skip the pointer field
		switch {
		case pk.pid == 0 && s.pat == nil:
			if pid := patPMT(sec); pid >= 0 {
				s.pmtPID, s.pat = pid, append([]byte(nil), raw...)
			}
		case pk.pid == s.pmtPID && s.pmt == nil:
			if pid, typ := pmtVideo(sec); pid >= 0 {
				s.videoPID, s.codec, s.pmt = pid, typ, append([]byte(nil), raw...)
			}
		}
		if s.pat != nil && s.pmt != nil {
			return s, nil
		}
	}
	return s, errors.New("no PAT and PMT with a video stream at the start of the file")
}

// patPMT is the PMT PID of the first program in a PAT section.
func patPMT(sec []byte) int {
	if len(sec) < 8 || sec[0] != 0 {
		return -1
	}
	end := min(3+(int(sec[1]&0x0f)<<8|int(sec[2]))-4, len(sec))
	for i := 8; i+4 <= end; i += 4 {
		if prog := int(sec[i])<<8 | int(sec[i+1]); prog != 0 {
			return int(sec[i+2]&0x1f)<<8 | int(sec[i+3])
		}
	}
	return -1
}

// pmtVideo is the PID and stream type of the first video stream in a PMT
// section.
func pmtVideo(sec []byte) (int, byte) {
	if len(sec) < 12 || sec[0] != 2 {
		return -1, 0
	}
	end := min(3+(int(sec[1]&0x0f)<<8|int(sec[2]))-4, len(sec))
	i := 12 + (int(sec[10]&0x0f)<<8 | int(sec[11]))
	for i+5 <= end {
		typ, pid := sec[i], int(sec[i+1]&0x1f)<<8|int(sec[i+2])
		switch typ {
		case 0x01, 0x02, 0x1b, 0x24: // MPEG-1/2, H.264, HEVC
			return pid, typ
		}
		i += 5 + (int(sec[i+3]&0x0f)<<8 | int(sec[i+4]))
	}
	return -1, 0
}

// pesPTS reads the PTS from the start of a PES packet.
func pesPTS(b []byte) (int64, bool) {
	if len(b) < 14 || b[0] != 0 || b[1] != 0 || b[2] != 1 || b[7]&0x80 == 0 {
		return 0, false
	}
	p := b[9:14]
	return int64(p[0]>>1&7)<<30 | int64(p[1])<<22 | int64(p[2]>>1)<<15 | int64(p[3])<<7 | int64(p[4]>>1), true
}

// shiftPacket takes shift (90 kHz ticks) off the PCR in the packet's
// adaptation field and off the PTS and DTS of a PES that starts in it.
func shiftPacket(p []byte, shift int64) {
	afc := p[3] >> 4 & 3
	i := 4
	if afc&2 != 0 {
		n := int(p[4])
		if n >= 7 && p[5]&0x10 != 0 { // PCR: 33 bits base, 6 reserved, 9 bits extension
			base := int64(p[6])<<25 | int64(p[7])<<17 | int64(p[8])<<9 | int64(p[9])<<1 | int64(p[10]>>7)
			base = (base - shift%ptsWrap + ptsWrap) % ptsWrap
			p[6], p[7], p[8], p[9] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1)
			p[10] = byte(base<<7) | p[10]&0x7f
		}
		i = 5 + n
	}
	if afc&1 == 0 || p[1]&0x40 == 0 || i+14 > packetSize {
		return
	}
	pes := p[i:packetSize]
	if pes[0] != 0 || pes[1] != 0 || pes[2] != 1 || pes[7]&0x80 == 0 {
		return
	}
	setTimestamp(pes[9:14], shift)
	if pes[7]&0x40 != 0 && len(pes) >= 19 {
		setTimestamp(pes[14:19], shift)
	}
}

// setTimestamp moves the 33-bit timestamp in five bytes back by shift, keeping
// the marker bits and the prefix.
func setTimestamp(b []byte, shift int64) {
	ts := int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
	ts = (ts - shift%ptsWrap + ptsWrap) % ptsWrap
	b[0] = b[0]&0xf0 | byte(ts>>30&7)<<1 | 1
	b[1] = byte(ts >> 22)
	b[2] = byte(ts>>14) | 1
	b[3] = byte(ts >> 7)
	b[4] = byte(ts<<1) | 1
}
