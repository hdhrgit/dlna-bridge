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
			skip := in % packetSize // a seek into the middle of a packet
			body, err := f.open(f.ctx, p.Off+in-skip, p.Len-in+skip)
			if err != nil {
				return 0, err
			}
			if p.Shift != 0 {
				body = &shifter{r: body, shift: p.Shift}
			}
			if _, err := io.CopyN(io.Discard, body, skip); err != nil {
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

// shifter passes a stream of whole packets through, moving the timestamps back.
type shifter struct {
	r     io.ReadCloser
	shift int64
	buf   []byte // converted bytes not yet read
	raw   [packetSize * 64]byte
}

func (s *shifter) Read(b []byte) (int, error) {
	if len(s.buf) == 0 {
		n, err := io.ReadFull(s.r, s.raw[:])
		n -= n % packetSize
		if n == 0 {
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			return 0, err
		}
		for i := 0; i < n; i += packetSize {
			if s.raw[i] == 0x47 {
				shiftPacket(s.raw[i:i+packetSize], s.shift)
			}
		}
		s.buf = s.raw[:n]
	}
	n := copy(b, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *shifter) Close() error { return s.r.Close() }
