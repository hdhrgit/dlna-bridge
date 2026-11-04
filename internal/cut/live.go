package cut

import (
	"io"
	gosync "sync"
)

// Live passes a live transport stream from a writer to a reader through a
// queue that holds no more than max of video (90 kHz ticks). When the reader
// is slower than the stream, the queue is cut back to its newest keyframe
// instead of falling further behind, and the timestamps after the cut are
// moved back by what was left out, so the stream plays on without a jump in
// time. It only cuts on keyframes that start a video PES with the random
// access flag set, as ffmpeg marks them.
type Live struct {
	max int64
	// OnCut, when set, is told of each cut and how much it left out.
	OnCut func(ticks int64)

	mu      gosync.Mutex
	more    *gosync.Cond
	chunks  []chunk // each but the first starts on a keyframe
	started bool    // the first chunk is partly read
	partial []byte  // the start of a packet not yet written in full
	pmtPID  int
	video   int
	shift   int64 // ticks taken off every timestamp from here on
	dropped int64 // ticks left out so far
	err     error
}

type chunk struct {
	pts  int64 // of its keyframe, or -1 when it does not start on one
	data []byte
}

func NewLive(max int64) *Live {
	l := &Live{max: max, pmtPID: -1, video: -1}
	l.more = gosync.NewCond(&l.mu)
	return l
}

// Write queues whole packets; a packet split across writes waits for its end.
func (l *Live) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return 0, l.err
	}
	n := len(b)
	if len(l.partial) > 0 {
		b = append(l.partial, b...)
		l.partial = nil
	}
	for len(b) >= packetSize {
		p := append([]byte(nil), b[:packetSize]...)
		b = b[packetSize:]
		if p[0] != 0x47 {
			continue
		}
		l.add(p)
	}
	l.partial = append([]byte(nil), b...)
	l.trim()
	l.more.Broadcast()
	return n, nil
}

func (l *Live) add(p []byte) {
	pk, _ := parsePacket(p)
	if pk.pusi && len(pk.payload) > 0 && 1+int(pk.payload[0]) < len(pk.payload) {
		sec := pk.payload[1+int(pk.payload[0]):]
		switch {
		case pk.pid == 0:
			if pid := patPMT(sec); pid >= 0 {
				l.pmtPID = pid
			}
		case pk.pid == l.pmtPID:
			if pid, _ := pmtVideo(sec); pid >= 0 {
				l.video = pid
			}
		}
	}
	if l.shift != 0 {
		shiftPacket(p, l.shift)
	}
	if pk.pid == l.video && pk.pusi && pk.rai {
		if pts, ok := pesPTS(pk.payload); ok {
			l.chunks = append(l.chunks, chunk{pts: pts})
		}
	}
	if len(l.chunks) == 0 {
		l.chunks = append(l.chunks, chunk{pts: -1})
	}
	c := &l.chunks[len(l.chunks)-1]
	c.data = append(c.data, p...)
}

// trim leaves out the whole chunks between the one being read and the newest
// when they hold more than max.
func (l *Live) trim() {
	first := 0
	if l.started || l.chunks[0].pts < 0 {
		first = 1 // being read, or the stream's start before its first keyframe
	}
	last := len(l.chunks) - 1
	if last <= first {
		return
	}
	gap := (l.chunks[last].pts - l.chunks[first].pts + ptsWrap) % ptsWrap
	if gap <= l.max {
		return
	}
	// The newest chunk was shifted as the earlier ones; now it follows on from
	// where the first dropped one began.
	shiftChunk(l.chunks[last].data, gap)
	l.chunks[last].pts = l.chunks[first].pts
	l.shift = (l.shift + gap) % ptsWrap
	l.dropped += gap
	l.chunks = append(l.chunks[:first], l.chunks[last])
	if l.OnCut != nil {
		l.OnCut(gap)
	}
}

func shiftChunk(b []byte, shift int64) {
	for off := 0; off+packetSize <= len(b); off += packetSize {
		shiftPacket(b[off:off+packetSize], shift)
	}
}

// Read gives the queued bytes in order, waiting for more while there are none.
func (l *Live) Read(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.chunks) == 0 || len(l.chunks[0].data) == 0 && len(l.chunks) == 1 {
		if l.err != nil {
			return 0, l.err
		}
		l.more.Wait()
	}
	if len(l.chunks[0].data) == 0 {
		l.chunks = l.chunks[1:]
		l.started = false
	}
	c := &l.chunks[0]
	n := copy(b, c.data)
	c.data = c.data[n:]
	l.started = true
	if len(c.data) == 0 && len(l.chunks) > 1 {
		l.chunks = l.chunks[1:]
		l.started = false
	}
	return n, nil
}

// Dropped is how much video has been left out, in 90 kHz ticks.
func (l *Live) Dropped() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped
}

// CloseWithError ends the stream: Read returns what is queued, then err (EOF
// when nil); Write returns err at once.
func (l *Live) CloseWithError(err error) {
	if err == nil {
		err = io.EOF
	}
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
	l.more.Broadcast()
}
