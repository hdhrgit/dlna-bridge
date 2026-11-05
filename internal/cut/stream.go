package cut

import (
	"context"
	"io"
)

// Stream is the cut recording from a time on, read once from start to end. It
// finds each break just before it is needed, so the first bytes go out after
// a few reads instead of after the whole plan. It has no length and no byte
// offsets; it is what answers a seek by time.
type Stream struct {
	ctx  context.Context
	pl   *planner
	open Opener
	logf func(format string, a ...any)
	cur  *File // the parts found so far that are not yet read
}

// NewStream starts the cut recording at t seconds into the recording (not
// into the cut version). logf, if set, hears of a break that cannot be found;
// the rest of the recording is then sent without cuts.
func NewStream(ctx context.Context, src Source, breaks []float64, t float64, open Opener, logf func(string, ...any)) (*Stream, error) {
	pl, err := newPlanner(ctx, src, breaks)
	if err != nil {
		return nil, err
	}
	if t > 0 {
		if err := pl.seek(t); err != nil {
			return nil, err
		}
	}
	return &Stream{ctx: ctx, pl: pl, open: open, logf: logf}, nil
}

func (s *Stream) Read(b []byte) (int, error) {
	for {
		if s.cur != nil {
			n, err := s.cur.Read(b)
			if err != io.EOF {
				return n, err
			}
			s.cur.Close()
			s.cur = nil
			if n > 0 {
				return n, nil
			}
		}
		parts, _, err := s.pl.next()
		if err == io.EOF {
			return 0, io.EOF
		}
		if err != nil {
			if s.ctx.Err() != nil {
				return 0, s.ctx.Err()
			}
			if s.logf != nil {
				s.logf("%v; sending the rest uncut", err)
			}
			if parts = s.pl.rest(); parts == nil {
				return 0, io.EOF
			}
		}
		p := &Plan{Parts: parts, videoPID: s.pl.st.videoPID}
		for _, part := range parts {
			p.Size += part.Len
		}
		s.cur = p.Open(s.ctx, s.open)
	}
}

func (s *Stream) Close() error {
	if s.cur != nil {
		s.cur.Close()
	}
	return nil
}
