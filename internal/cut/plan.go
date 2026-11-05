package cut

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
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
	// Shift is what to take off every timestamp and clock mark (90 kHz) in the
	// part, so the time keeps running across the break before it.
	Shift int64
	// Video packets from DropFrom to DropTo, counted from the start of the
	// part, are the pictures right after the first keyframe that show what came
	// before it. Without that footage they decode as garbage, so they are
	// replaced by empty packets.
	DropFrom, DropTo int64
}

// Plan is the recording with the breaks left out.
type Plan struct {
	Parts    []Part
	videoPID int
	Size     int64
	// Kept lists the time ranges, in the recording, that the plan keeps.
	Kept [][2]float64
}

// A recording whose video timestamps jump (a splice, a restart of the encoder)
// does not keep time with the DVR's marks, and cutting by them would remove the
// wrong stretches. The frame found for a mark must be near the mark: a frame
// shown within maxOutSlack seconds after where a break starts, and a keyframe
// within maxInSlack after where it ends.
const (
	maxOutSlack = 5
	maxInSlack  = 20
)

// ErrTimeline is the error for such a recording. The caller serves it whole.
var ErrTimeline = errors.New("the video's timestamps do not keep time with the recording")

// probe is how much is read at each step of the search. At a few Mbps that
// holds several video frames, so one read nearly always finds a timestamp.
const probe = 128 << 10

// Make finds where each break starts and ends in the file. breaks is the DVR's
// list: start, end, start, end... in seconds from the start of the recording.
func Make(ctx context.Context, src Source, breaks []float64) (*Plan, error) {
	if len(breaks)%2 != 0 {
		return nil, fmt.Errorf("odd list of commercial marks (%d)", len(breaks))
	}
	src = newBlockCache(src)
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
	f.note(0, int64(first))
	size := src.Size()
	// A frame near the end, so that every search has bounds.
	if tail := f.align(max(size-2<<20, int64(first))); tail > int64(first) {
		if pts, at, err := f.ptsAfter(tail); err == nil {
			f.note(f.seconds(pts), at)
		}
	}
	p := &Plan{videoPID: st.videoPID}
	pos, posT := int64(0), 0.0 // the start of the part being kept
	var shift int64            // the length of the breaks so far, in 90 kHz ticks
	var dropFrom, dropTo int64 // the leading pictures after pos
	for i := 0; i < len(breaks); i += 2 {
		from, to := breaks[i], breaks[i+1]
		if to <= from || from < posT {
			continue
		}
		out, outT, err := f.packetAt(from)
		if err != nil {
			return nil, fmt.Errorf("break at %.1fs: %w", from, err)
		}
		if outT-from > maxOutSlack {
			return nil, fmt.Errorf("break at %.1fs: the video there is at %.1fs: %w", from, outT, ErrTimeline)
		}
		in, inT, err := f.keyframeAfter(to)
		if errors.Is(err, io.EOF) {
			in, inT = size, to // the break runs to the end
		} else if err != nil {
			return nil, fmt.Errorf("end of break at %.1fs: %w", to, err)
		} else if inT-to > maxInSlack {
			return nil, fmt.Errorf("end of break at %.1fs: the next keyframe is at %.1fs: %w", to, inT, ErrTimeline)
		}
		if out > pos {
			if pos > 0 {
				p.Parts = append(p.Parts, Part{Data: append(append([]byte(nil), st.pat...), st.pmt...)})
			}
			p.Parts = append(p.Parts, Part{Off: pos, Len: out - pos, Shift: shift, DropFrom: dropFrom, DropTo: dropTo})
			p.Kept = append(p.Kept, [2]float64{posT, from})
		}
		if in < size {
			shift += int64(math.Round((inT - outT) * 90000))
		}
		pos, posT = in, inT
		dropFrom, dropTo = 0, 0
		if in < size {
			if dropFrom, dropTo, err = f.leading(in, inT); err != nil {
				return nil, fmt.Errorf("after the break at %.1fs: %w", from, err)
			}
		}
	}
	if pos < size {
		if pos > 0 {
			p.Parts = append(p.Parts, Part{Data: append(append([]byte(nil), st.pat...), st.pmt...)})
		}
		p.Parts = append(p.Parts, Part{Off: pos, Len: size - pos, Shift: shift, DropFrom: dropFrom, DropTo: dropTo})
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

// planJSON is a Plan as kept on disk.
type planJSON struct {
	Parts    []Part       `json:"parts"`
	Size     int64        `json:"size"`
	Kept     [][2]float64 `json:"kept"`
	VideoPID int          `json:"video_pid"`
}

func (p *Plan) MarshalJSON() ([]byte, error) {
	return json.Marshal(planJSON{p.Parts, p.Size, p.Kept, p.videoPID})
}

func (p *Plan) UnmarshalJSON(b []byte) error {
	var j planJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	*p = Plan{Parts: j.Parts, Size: j.Size, Kept: j.Kept, videoPID: j.VideoPID}
	return nil
}

// Valid says whether the plan fits a recording of fileSize bytes: its parts lie
// inside the file, are whole packets, and add up to the size it claims. A plan
// read back from disk is checked before it is used.
func (p *Plan) Valid(fileSize int64) bool {
	var total int64
	for _, part := range p.Parts {
		if part.Len <= 0 || (part.Data != nil && part.Len%packetSize != 0) {
			return false // a part of the recording may hold damaged packets
		}
		if part.Data == nil && (part.Off < 0 || part.Off+part.Len > fileSize) {
			return false
		}
		total += part.Len
	}
	return len(p.Parts) > 0 && total == p.Size
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
	// samples are the frames seen so far: the video at byte off is shown at
	// time t. Both only grow along the file, which lets a search for another time
	// start from the nearest two instead of from the whole file.
	samples []sample
}

type sample struct {
	t   float64
	off int64
}

// note remembers that the frame at off is shown at t.
func (f *finder) note(t float64, off int64) {
	i := sort.Search(len(f.samples), func(i int) bool { return f.samples[i].off >= off })
	if i < len(f.samples) && f.samples[i].off == off {
		return
	}
	f.samples = append(f.samples, sample{})
	copy(f.samples[i+1:], f.samples[i:])
	f.samples[i] = sample{t, off}
}

// bracket gives, for a time t, the offset just past a frame shown before t (lo,
// at time tlo) and the offset of a frame shown at or after t (hi, at time thi).
// There is no hi when no frame seen is that late.
func (f *finder) bracket(t float64) (lo int64, tlo float64, hi int64, thi float64, haveHi bool) {
	lo, tlo = f.first, 0
	for _, s := range f.samples {
		if s.t < t {
			lo, tlo = s.off+packetSize, s.t
		} else {
			return lo, tlo, s.off, s.t, s.off > lo
		}
	}
	return lo, tlo, 0, 0, false
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
		next := len(b) - 3*packetSize // where to read on from, past damage
		for i := resync(b, 0); i >= 0; i = resync(b, i+packetSize) {
			next = i + packetSize
			pk, _ := parsePacket(b[i:])
			if pk.pid == f.st.videoPID && pk.pusi {
				if pts, ok := pesPTS(pk.payload); ok {
					return pts, off + int64(i), nil
				}
			}
		}
		if off+int64(len(b)) >= f.src.Size() {
			return 0, 0, io.EOF
		}
		off += int64(max(next, packetSize))
	}
	return 0, 0, errors.New("no video timestamp found")
}

// seconds is a PTS as time from the start of the recording.
func (f *finder) seconds(pts int64) float64 {
	return float64((pts-f.base+ptsWrap)%ptsWrap) / 90000
}

// packetAt finds the first video frame at or after t seconds. Frames seen
// before bound it. Inside the bounds it estimates where t is from how many bytes
// a second the video has been running at, reads there, and corrects the
// estimate from what it finds: a few reads where halving the file takes a dozen.
// Once close it aims a little to the other side of t, so that the bounds close
// from both sides. It falls back to halving if that does not settle.
func (f *finder) packetAt(t float64) (int64, float64, error) {
	lo, tlo, hi, thi, haveHi := f.bracket(t)
	if !haveHi {
		return f.scan(lo, t, false) // later than any frame seen: near the end
	}
	rate := 1e6 // bytes a second
	if thi > tlo {
		rate = float64(hi-lo) / (thi - tlo)
	}
	var last sample // the frame read last, when there is one
	haveLast := false
	for iter := 0; hi-lo > probe && iter < 40; iter++ {
		var g int64
		switch {
		case iter >= 12: // not settling: halve
			g = lo + (hi-lo)/2
		case haveLast:
			aim := t
			if d := last.t - t; d > -3 && d < 3 {
				// Close: aim past t on the other side of the frame just read.
				if d >= 0 {
					aim = t - 0.3
				} else {
					aim = t + 0.3
				}
			}
			g = last.off + int64((aim-last.t)*rate)
		default:
			// From the bracket end nearer to t.
			if t-tlo <= thi-t {
				g = lo + int64((t-tlo)*rate)
			} else {
				g = hi - int64((thi-t)*rate)
			}
		}
		margin := int64(probe / 2)
		g = min(max(g, lo+margin), hi-margin)
		pts, at, err := f.ptsAfter(g)
		if errors.Is(err, io.EOF) {
			hi = f.align(g)
			continue
		}
		if err != nil {
			return 0, 0, err
		}
		tg := f.seconds(pts)
		f.note(tg, at)
		if haveLast && math.Abs(tg-last.t) > 0.05 && at != last.off {
			if r := float64(at-last.off) / (tg - last.t); r > 0 {
				rate = r
			}
		}
		last, haveLast = sample{tg, at}, true
		if tg < t {
			lo, tlo = at+packetSize, tg
		} else {
			hi, thi = at, tg
		}
	}
	return f.scan(lo, t, false)
}

// keyframeAfter finds the first keyframe at or after t seconds, and its time.
func (f *finder) keyframeAfter(t float64) (int64, float64, error) {
	off, _, err := f.packetAt(t)
	if err != nil {
		return 0, 0, err
	}
	return f.scan(off, t, true)
}

// scanChunk is how much a scan asks for at a time. It reads on in order, which
// costs the DVR less than the same bytes in small reads from several places.
const scanChunk = 256 << 10

// scan reads on from off to the first video frame at or after t seconds (a
// keyframe, if asked). Keyframes come every few seconds; it gives up after
// 16 MB.
func (f *finder) scan(off int64, t float64, keyframe bool) (int64, float64, error) {
	off = f.align(off)
	for end := off + 16<<20; off < end; {
		if off >= f.src.Size() {
			return 0, 0, io.EOF
		}
		b, err := f.src.ReadAt(f.ctx, off, scanChunk)
		if err != nil {
			return 0, 0, err
		}
		next := len(b) - 3*packetSize
		for i := resync(b, 0); i >= 0; i = resync(b, i+packetSize) {
			next = i + packetSize
			pk, _ := parsePacket(b[i:])
			if pk.pid != f.st.videoPID || !pk.pusi {
				continue
			}
			pts, ok := pesPTS(pk.payload)
			if !ok || f.seconds(pts) < t {
				continue
			}
			if !keyframe || pk.rai || keyframeNAL(pesHead(b, i, f.st.videoPID), f.st.hevc) {
				return off + int64(i), f.seconds(pts), nil
			}
		}
		if off+int64(len(b)) >= f.src.Size() {
			return 0, 0, io.EOF
		}
		off += int64(max(next, packetSize))
	}
	return 0, 0, fmt.Errorf("nothing found within 16 MB after %.1fs", t)
}

// leading finds the pictures that follow the keyframe at off in the file but
// are shown before it (B-frames of an open GOP): the byte range, counted from
// off, that holds their video packets. It stops at the first picture shown
// after the keyframe.
func (f *finder) leading(off int64, keyT float64) (from, to int64, err error) {
	start := off
	for end := off + 4<<20; off < end; {
		if off >= f.src.Size() {
			return from, to, nil
		}
		b, err := f.src.ReadAt(f.ctx, off, scanChunk)
		if err != nil {
			return 0, 0, err
		}
		next := len(b) - 3*packetSize
		for i := resync(b, 0); i >= 0; i = resync(b, i+packetSize) {
			next = i + packetSize
			pk, _ := parsePacket(b[i:])
			if pk.pid != f.st.videoPID || !pk.pusi || off+int64(i) == start {
				continue
			}
			pts, ok := pesPTS(pk.payload)
			if !ok {
				continue
			}
			at := off + int64(i) - start
			if f.seconds(pts) >= keyT {
				if from > 0 {
					to = at
				}
				return from, to, nil
			}
			if from == 0 {
				from = at
			}
		}
		if off+int64(len(b)) >= f.src.Size() {
			return from, to, nil
		}
		off += int64(max(next, packetSize))
	}
	return 0, 0, nil // no end in sight: leave the pictures in
}

// pesHead is the start of the PES that begins in packet i of b: the rest of
// its first 1 kB, taken from the packets that follow for the same stream, since
// the headers before a keyframe's slice (a picture's delimiter, SPS, PPS,
// SEI) often run past the first packet.
func pesHead(b []byte, i int, pid int) []byte {
	pk, ok := parsePacket(b[i:])
	if !ok {
		return nil
	}
	head := append([]byte(nil), pk.payload...)
	for j := i + packetSize; j+packetSize <= len(b) && len(head) < 1024; j += packetSize {
		next, ok := parsePacket(b[j:])
		if !ok || (next.pid == pid && next.pusi) {
			break
		}
		if next.pid == pid {
			head = append(head, next.payload...)
		}
	}
	return head
}

// keyframeNAL looks in the first packet of a PES for an H.264 SPS or IDR, or
// an HEVC VPS, SPS or IRAP picture. MPEG-2 streams rely on the random access
// flag alone.
func keyframeNAL(pes []byte, hevc bool) bool {
	if len(pes) < 9 || 9+int(pes[8]) > len(pes) {
		return false // the header runs on into the next packet: leave it to the keyframe flag
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
