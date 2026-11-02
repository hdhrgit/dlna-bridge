package cut

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// Source reads byte ranges of the recording.
type Source interface {
	ReadAt(ctx context.Context, off, n int64) ([]byte, error)
	Size() int64
}

// Part is a run of the recording's bytes, or bytes to put in between.
type Part struct {
	Off, Len int64  // in the recording
	Data     []byte // when set, served instead of the recording's bytes
}

// Plan is the recording with the breaks left out.
type Plan struct {
	Parts []Part
	Size  int64
	// Kept lists the time ranges, in the recording, that the plan keeps.
	Kept [][2]float64
}

// probe is how much is read at each step of the search. At a few Mbps that
// holds several video frames, so one read nearly always finds a timestamp.
const probe = 128 << 10

// Make finds where each break starts and ends in the file. breaks is the DVR's
// list: start, end, start, end... in seconds from the start of the recording.
func Make(ctx context.Context, src Source, breaks []float64) (*Plan, error) {
	if len(breaks)%2 != 0 {
		return nil, fmt.Errorf("odd list of commercial marks (%d)", len(breaks))
	}
	head, err := src.ReadAt(ctx, 0, 1<<20)
	if err != nil {
		return nil, err
	}
	st, err := findStreams(head)
	if err != nil {
		return nil, err
	}
	first := sync(head)
	f := &finder{ctx: ctx, src: src, st: st, first: int64(first)}
	base, _, err := f.ptsAfter(int64(first))
	if err != nil {
		return nil, err
	}
	f.base = base

	size := src.Size()
	p := &Plan{}
	pos, posT := int64(0), 0.0 // the start of the part being kept
	for i := 0; i < len(breaks); i += 2 {
		from, to := breaks[i], breaks[i+1]
		if to <= from || from < posT {
			continue
		}
		out, err := f.packetAt(from)
		if err != nil {
			return nil, fmt.Errorf("break at %.1fs: %w", from, err)
		}
		in, inT, err := f.keyframeAfter(to)
		if errors.Is(err, io.EOF) {
			in, inT = size, to // the break runs to the end
		} else if err != nil {
			return nil, fmt.Errorf("end of break at %.1fs: %w", to, err)
		}
		if out > pos {
			if pos > 0 {
				p.Parts = append(p.Parts, Part{Data: append(append([]byte(nil), st.pat...), st.pmt...)})
			}
			p.Parts = append(p.Parts, Part{Off: pos, Len: out - pos})
			p.Kept = append(p.Kept, [2]float64{posT, from})
		}
		pos, posT = in, inT
	}
	if pos < size {
		if pos > 0 {
			p.Parts = append(p.Parts, Part{Data: append(append([]byte(nil), st.pat...), st.pmt...)})
		}
		p.Parts = append(p.Parts, Part{Off: pos, Len: size - pos})
		p.Kept = append(p.Kept, [2]float64{posT, -1})
	}
	for i := range p.Parts {
		if p.Parts[i].Data != nil {
			p.Parts[i].Len = int64(len(p.Parts[i].Data))
		}
		p.Size += p.Parts[i].Len
	}
	return p, nil
}

// CutTime turns a time in the recording into the time in the cut version.
func (p *Plan) CutTime(t float64) float64 {
	var out float64
	for _, k := range p.Kept {
		end := k[1]
		if end < 0 || t < end {
			if t > k[0] {
				out += t - k[0]
			}
			return out
		}
		out += end - k[0]
	}
	return out
}

type finder struct {
	ctx   context.Context
	src   Source
	st    streams
	first int64 // offset of the first packet
	base  int64 // PTS of the first video frame
}

// align rounds an offset down to a packet boundary.
func (f *finder) align(off int64) int64 {
	if off < f.first {
		return f.first
	}
	return off - (off-f.first)%packetSize
}

// ptsAfter is the first video PTS at or after off, and the offset of its packet.
func (f *finder) ptsAfter(off int64) (int64, int64, error) {
	off = f.align(off)
	for range 16 {
		if off >= f.src.Size() {
			return 0, 0, io.EOF
		}
		b, err := f.src.ReadAt(f.ctx, off, probe)
		if err != nil {
			return 0, 0, err
		}
		for i := 0; i+packetSize <= len(b); i += packetSize {
			pk, ok := parsePacket(b[i:])
			if !ok {
				return 0, 0, fmt.Errorf("lost packet sync at byte %d", off+int64(i))
			}
			if pk.pid == f.st.videoPID && pk.pusi {
				if pts, ok := pesPTS(pk.payload); ok {
					return pts, off + int64(i), nil
				}
			}
		}
		off += int64(len(b) / packetSize * packetSize)
	}
	return 0, 0, errors.New("no video timestamp found")
}

// seconds is a PTS as time from the start of the recording.
func (f *finder) seconds(pts int64) float64 {
	return float64((pts-f.base+ptsWrap)%ptsWrap) / 90000
}

// packetAt finds the first video frame at or after t seconds, by bisecting
// the file on timestamps and then reading on from there.
func (f *finder) packetAt(t float64) (int64, error) {
	lo, hi := f.first, f.src.Size()
	for hi-lo > probe {
		mid := f.align(lo + (hi-lo)/2)
		pts, at, err := f.ptsAfter(mid)
		if errors.Is(err, io.EOF) {
			hi = mid
			continue
		}
		if err != nil {
			return 0, err
		}
		if f.seconds(pts) < t {
			lo = at + packetSize
		} else {
			hi = mid
		}
	}
	off, _, err := f.scan(lo, t, false)
	return off, err
}

// keyframeAfter finds the first keyframe at or after t seconds, and its time.
func (f *finder) keyframeAfter(t float64) (int64, float64, error) {
	off, err := f.packetAt(t)
	if err != nil {
		return 0, 0, err
	}
	return f.scan(off, t, true)
}

// scan reads on from off to the first video frame at or after t seconds (a
// keyframe, if asked). Keyframes come every few seconds; it gives up after
// 16 MB.
func (f *finder) scan(off int64, t float64, keyframe bool) (int64, float64, error) {
	off = f.align(off)
	for end := off + 16<<20; off < end; {
		if off >= f.src.Size() {
			return 0, 0, io.EOF
		}
		b, err := f.src.ReadAt(f.ctx, off, probe)
		if err != nil {
			return 0, 0, err
		}
		for i := 0; i+packetSize <= len(b); i += packetSize {
			pk, ok := parsePacket(b[i:])
			if !ok {
				return 0, 0, fmt.Errorf("lost packet sync at byte %d", off+int64(i))
			}
			if pk.pid != f.st.videoPID || !pk.pusi {
				continue
			}
			pts, ok := pesPTS(pk.payload)
			if !ok || f.seconds(pts) < t {
				continue
			}
			if !keyframe || pk.rai || keyframeNAL(pk.payload, f.st.hevc) {
				return off + int64(i), f.seconds(pts), nil
			}
		}
		off += int64(len(b) / packetSize * packetSize)
	}
	return 0, 0, fmt.Errorf("nothing found within 16 MB after %.1fs", t)
}

// keyframeNAL looks in the first packet of a PES for an H.264 SPS or IDR, or
// an HEVC VPS, SPS or IRAP picture. MPEG-2 streams rely on the random access
// flag alone.
func keyframeNAL(pes []byte, hevc bool) bool {
	if len(pes) < 9 {
		return false
	}
	b := pes[9+int(pes[8]):]
	for {
		i := bytes.Index(b, []byte{0, 0, 1})
		if i < 0 || i+4 > len(b) {
			return false
		}
		h := b[i+3]
		if hevc {
			if t := h >> 1 & 0x3f; t == 32 || t == 33 || (t >= 16 && t <= 21) {
				return true
			}
		} else if t := h & 0x1f; t == 5 || t == 7 {
			return true
		}
		b = b[i+3:]
	}
}
