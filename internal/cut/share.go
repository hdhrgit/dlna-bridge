package cut

import (
	"io"
	gosync "sync"
)

// gopLimit is the most held from the newest keyframe for readers to start at.
// A stream with no keyframe that often has none to start at until its next.
const gopLimit = 8 << 20

// frameTicks is the gap put between the last frame from one share and the
// first after it when a reader skips: one frame at 30 fps, in 90 kHz.
const frameTicks = 3000

// Share hands one live transport stream to any number of readers. A new
// reader starts at the newest keyframe, after the newest PAT and PMT, so that
// a decoder can begin there. Writing never waits for a reader: one that falls
// too far behind skips to the next keyframe, with its timestamps shifted to
// follow on from what it read, so that it plays on without a jump in time. A
// stream with no video starts at any PES of its sound.
type Share struct {
	mu      gosync.Mutex
	partial []byte
	pat     []byte
	pmt     []byte
	pmtPID  int
	video   int
	clock   int    // the PID whose PTS a reader follows on from: the video's, or the sound's
	gop     []byte // from the newest keyframe on; nil until there is one
	ready   chan struct{}
	readers map[*ShareReader]struct{}
	err     error
}

func NewShare() *Share {
	return &Share{pmtPID: -1, video: -1, clock: -1, ready: make(chan struct{}), readers: map[*ShareReader]struct{}{}}
}

// Ready is closed once the stream has a keyframe for readers to start at.
func (s *Share) Ready() <-chan struct{} { return s.ready }

// Readers is how many readers the share has.
func (s *Share) Readers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.readers)
}

// Write takes whole packets; a packet split across writes waits for its end.
func (s *Share) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	n := len(b)
	if len(s.partial) > 0 {
		b = append(s.partial, b...)
		s.partial = nil
	}
	for len(b) >= packetSize {
		p := b[:packetSize]
		b = b[packetSize:]
		if p[0] == 0x47 {
			s.add(p)
		}
	}
	s.partial = append([]byte(nil), b...)
	return n, nil
}

func (s *Share) add(p []byte) {
	pk, _ := parsePacket(p)
	if pk.pusi && len(pk.payload) > 0 && 1+int(pk.payload[0]) < len(pk.payload) {
		sec := pk.payload[1+int(pk.payload[0]):]
		switch {
		case pk.pid == 0:
			if pid := patPMT(sec); pid >= 0 {
				s.pmtPID, s.pat = pid, append(s.pat[:0], p...)
			}
		case pk.pid == s.pmtPID:
			s.video, _ = pmtVideo(sec)
			if s.video >= 0 {
				s.clock = s.video
			}
			s.pmt = append(s.pmt[:0], p...)
		}
	}
	var key bool
	switch {
	case s.pat == nil || s.pmt == nil || !pk.pusi:
	case s.video >= 0:
		key = pk.pid == s.video && pk.rai
	default: // sound alone
		if pk.pid != 0 && pk.pid != s.pmtPID {
			if s.clock < 0 {
				s.clock = pk.pid
			}
			key = pk.pid == s.clock
		}
	}
	if key {
		s.gop = append(s.gop[:0], p...)
		select {
		case <-s.ready:
		default:
			close(s.ready)
		}
	} else if s.gop != nil {
		if len(s.gop) >= gopLimit {
			s.gop = nil
		} else {
			s.gop = append(s.gop, p...)
		}
	}
	for r := range s.readers {
		if !r.packet(p, pk, s.clock, key) {
			delete(s.readers, r)
		}
	}
}

// Reader starts a reader at the newest keyframe; it reads nothing until the
// stream has one. max is the most it may let pile up before it skips ahead;
// onSkip, if set, is told of each skip.
func (s *Share) Reader(max int, onSkip func()) *ShareReader {
	r := &ShareReader{max: max, clock: -1, onSkip: onSkip}
	r.more = gosync.NewCond(&r.mu)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		r.err = s.err
		return r
	}
	r.share = s
	r.clock = s.clock
	if s.gop != nil {
		r.start(s, s.gop)
	} else {
		r.waitKey = true
	}
	s.readers[r] = struct{}{}
	return r
}

// CloseWithError ends the stream: readers get what they have, then err (EOF
// when nil).
func (s *Share) CloseWithError(err error) {
	if err == nil {
		err = io.EOF
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
	for r := range s.readers {
		r.mu.Lock()
		if r.err == nil {
			r.err = err
		}
		r.more.Broadcast()
		r.mu.Unlock()
	}
	s.readers = map[*ShareReader]struct{}{}
}

// ShareReader is one reader of a Share.
type ShareReader struct {
	mu      gosync.Mutex
	more    *gosync.Cond
	share   *Share // guarded by the share's lock
	buf     []byte
	max     int
	onSkip  func()
	read    int64 // bytes taken by Read so far
	queued  int64 // bytes queued so far
	err     error
	clock   int
	waitKey bool // nothing is passed on until the next keyframe
	follow  bool // and then the timestamps are shifted to follow on
	skips   int
	shift   int64 // taken off every timestamp
	marks   []mark
	last    int64 // the newest PTS on the clock PID queued, after the shift
	sent    int64 // and the newest read
	hasLast bool
}

// mark is where in the queue a PES of the clock PID starts, and its PTS.
type mark struct {
	at  int64
	pts int64
}

// start queues the PAT, PMT and b, which begins on a keyframe. The share's
// lock is held.
func (r *ShareReader) start(s *Share, b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue(s.pat, -1)
	r.queue(s.pmt, -1)
	for off := 0; off+packetSize <= len(b); off += packetSize {
		r.queue(b[off:off+packetSize], s.clock)
	}
	r.more.Broadcast()
}

// queue adds one packet, shifted, and notes its PTS if it is on clock.
func (r *ShareReader) queue(p []byte, clock int) {
	if len(p) < packetSize {
		return
	}
	at := len(r.buf)
	r.buf = append(r.buf, p...)
	q := r.buf[at:]
	if r.shift != 0 {
		shiftPacket(q, r.shift)
	}
	if pk, _ := parsePacket(q); pk.pid == clock && pk.pusi {
		if pts, ok := pesPTS(pk.payload); ok && (!r.hasLast || later(pts, r.last)) {
			r.last, r.hasLast = pts, true
			r.marks = append(r.marks, mark{r.queued, pts})
		}
	}
	r.queued += packetSize
}

// later says whether timestamp a comes after b.
func later(a, b int64) bool { return (a-b+ptsWrap)%ptsWrap < ptsWrap/2 }

// packet queues one packet, and says whether the reader is still there. The
// share's lock is held.
func (r *ShareReader) packet(p []byte, pk packet, clock int, key bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false
	}
	if r.waitKey {
		if !key {
			return true
		}
		r.waitKey = false
		r.clock = clock
		if r.follow {
			r.follow = false
			if pts, ok := pesPTS(pk.payload); ok && r.hasLast {
				r.shift = (pts - r.last - frameTicks + 2*ptsWrap) % ptsWrap
			}
		}
		r.queue(r.share.pat, -1)
		r.queue(r.share.pmt, -1)
	}
	r.queue(p, clock)
	if len(r.buf) > r.max {
		// Keep the rest of a packet being read, so that what follows
		// still starts on a packet, and follow on from what was read.
		keep := (packetSize - int(r.read%packetSize)) % packetSize
		r.queued -= int64(len(r.buf) - keep)
		r.buf = r.buf[:keep]
		r.marks = nil
		r.last = r.sent
		r.waitKey, r.follow = true, true
		r.skips++
		if r.onSkip != nil {
			r.onSkip()
		}
	}
	r.more.Broadcast()
	return true
}

// Skips is how many times the reader has skipped ahead.
func (r *ShareReader) Skips() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.skips
}

// Read gives the queued bytes, waiting for more while there are none.
func (r *ShareReader) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.buf) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		r.more.Wait()
	}
	n := copy(b, r.buf)
	r.buf = r.buf[n:]
	r.read += int64(n)
	for len(r.marks) > 0 && r.marks[0].at < r.read {
		r.sent = r.marks[0].pts
		r.marks = r.marks[1:]
	}
	if len(r.buf) == 0 {
		r.buf = nil
	}
	return n, nil
}

// Close leaves the share; a Read waiting returns io.EOF.
func (r *ShareReader) Close() error {
	r.mu.Lock()
	s := r.share
	r.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		delete(s.readers, r)
		s.mu.Unlock()
	}
	r.mu.Lock()
	if r.err == nil {
		r.err = io.EOF
	}
	r.buf = nil
	r.more.Broadcast()
	r.mu.Unlock()
	return nil
}
