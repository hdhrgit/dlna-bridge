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
			body, err := f.open(f.ctx, p.Off+in, p.Len-in)
			if err != nil {
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
