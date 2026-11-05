package cut

import (
	"context"
	"errors"
	"io"
)

// Opener streams the recording's bytes from off, to the end of the file.
type Opener func(ctx context.Context, off, n int64) (io.ReadCloser, error)

// File is the cut recording as an io.ReadSeeker, for http.ServeContent. It
// reads each part of the recording with one request.
type File struct {
	ctx  context.Context
	plan *Plan
	open Opener
	pos  int64

	body    io.ReadCloser
	bodyPos int64 // where in the cut file body's next byte belongs
}

func (p *Plan) Open(ctx context.Context, open Opener) *File {
	return &File{ctx: ctx, plan: p, open: open}
}

func (f *File) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += f.pos
	case io.SeekEnd:
		off += f.plan.Size
	}
	if off < 0 {
		return 0, errors.New("seek before the start")
	}
	f.pos = off
	return off, nil
}

func (f *File) Read(b []byte) (int, error) {
	if f.pos >= f.plan.Size {
		return 0, io.EOF
	}
	// Find the part holding pos.
	start := int64(0)
	for _, p := range f.plan.Parts {
		if f.pos >= start+p.Len {
			start += p.Len
			continue
		}
		in := f.pos - start // offset into the part
		if p.Data != nil {
			n := copy(b, p.Data[in:])
			f.pos += int64(n)
			return n, nil
		}
		if f.body == nil || f.bodyPos != f.pos {
			f.closeBody()
			// The shifter changes whole packets, so it starts a little before
			// pos, where it can find the packet holding pos even if damage has
			// moved the packets off the grid; the bytes before pos are dropped.
			// Bytes passed as they are can start at pos itself.
			from := in
			shifted := p.Shift != 0 || p.DropTo > 0
			if shifted {
				from = max(0, in-in%packetSize-2*packetSize)
			}
			body, err := f.open(f.ctx, p.Off+from, p.Len-from)
			if err != nil {
				return 0, err
			}
			if shifted {
				body = &shifter{r: body, shift: p.Shift, pos: from, dropFrom: p.DropFrom, dropTo: p.DropTo, videoPID: f.plan.videoPID}
			}
			if _, err := io.CopyN(io.Discard, body, in-from); err != nil {
				body.Close()
				return 0, err
			}
			f.body, f.bodyPos = body, f.pos
		}
		if rest := p.Len - in; int64(len(b)) > rest {
			b = b[:rest]
		}
		n, err := f.body.Read(b)
		f.pos += int64(n)
		f.bodyPos = f.pos
		if err == io.EOF || f.pos == start+p.Len {
			f.closeBody()
			if n == 0 {
				return 0, io.ErrUnexpectedEOF // the recording is shorter than planned
			}
			err = nil
		}
		return n, err
	}
	return 0, io.EOF
}

func (f *File) closeBody() {
	if f.body != nil {
		f.body.Close()
		f.body = nil
	}
}

func (f *File) Close() error {
	f.closeBody()
	return nil
}

// shifter passes a stream of packets through, moving the timestamps back. Bytes
// that are not a whole packet (one cut short by damage) pass as they are, and
// the packets after them are found again.
type shifter struct {
	r                io.ReadCloser
	shift            int64
	pos              int64 // of buf's first byte in the part
	dropFrom, dropTo int64
	videoPID         int
	raw              []byte // read, not yet looked at
	buf              []byte // looked at, not yet returned
	eof              bool
	err              error
}

// shifterRead is how much the shifter reads at a time.
const shifterRead = packetSize * 64

func (s *shifter) Read(b []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		s.fill()
	}
	n := copy(b, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// fill reads on and makes the next run of bytes ready in buf.
func (s *shifter) fill() {
	// Two packets past the one looked at, to tell whether it is whole.
	for !s.eof && len(s.raw) < shifterRead+2*packetSize {
		chunk := make([]byte, shifterRead)
		n, err := io.ReadFull(s.r, chunk)
		s.raw = append(s.raw, chunk[:n]...)
		if err != nil {
			s.eof = true
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				s.err = err
			}
		}
	}
	keep := 0 // what stays in raw: bytes too near the end to judge yet
	if !s.eof {
		keep = 2 * packetSize
	}
	out := s.raw[:0:0]
	i := 0
	for i < len(s.raw)-keep {
		j := resync(s.raw, i)
		if j < 0 {
			if s.eof {
				j = len(s.raw)
			} else {
				j = len(s.raw) - keep
			}
		}
		if j > i { // not a packet: as it is
			out = append(out, s.raw[i:j]...)
			s.pos += int64(j - i)
			i = j
			continue
		}
		p := append([]byte(nil), s.raw[i:i+packetSize]...)
		if s.pos >= s.dropFrom && s.pos < s.dropTo && (int(p[1]&0x1f)<<8|int(p[2])) == s.videoPID {
			nullPacket(p)
		} else {
			shiftPacket(p, s.shift)
		}
		out = append(out, p...)
		s.pos += packetSize
		i += packetSize
	}
	s.raw = append([]byte(nil), s.raw[i:]...)
	s.buf = out
	if len(out) == 0 && s.eof && s.err == nil {
		s.err = io.EOF
	}
}

func (s *shifter) Close() error { return s.r.Close() }

// nullPacket turns a packet into a null packet, which decoders skip.
func nullPacket(p []byte) {
	p[1], p[2], p[3] = 0x1f, 0xff, 0x10
	for i := 4; i < packetSize; i++ {
		p[i] = 0xff
	}
}
