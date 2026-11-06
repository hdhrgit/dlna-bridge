package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
)

// Where a TV left a recording goes to the DVR, as the Channels apps do, so
// that either resumes where the other stopped, and one played to the end is
// marked watched. A Samsung says where it stopped (X_SetBookmark); other TVs
// do not, and the bridge works it out from how far into the file the stream
// had gone when the TV hung up.

const (
	minWatch    = 10 * time.Second // a shorter stream is a TV probing, not playing
	minWatchMB  = 4 << 20
	readAhead   = 10 // seconds a TV has read past what it shows
	minResume   = 60 // seconds: an earlier stop is not worth resuming
	watchedTail = 60 // seconds from the end that count as the end
)

// streamPosition notes where a stream of a recording got to: start is the byte
// it began at, sent the bytes sent, size the stream's length. For a recording
// with its breaks cut, the times are of the cut version, and the DVR is told the
// time in the recording.
func (s *Server) streamPosition(r *http.Request, f dvr.Recording, cut bool, start, sent, size int64, began time.Time) {
	if _, prof := s.profileFor(r); prof.Playback == config.PlaybackKeep || r.Method != http.MethodGet || size <= 0 || time.Since(began) < minWatch || sent < minWatchMB {
		return
	}
	length := f.Duration
	if cut {
		length = f.AdFree(-1)
	}
	t := float64(start+sent)/float64(size)*length - readAhead
	s.playedTo(r, f, cut, t, length)
}

// bookmarkToDVR passes a Samsung's bookmark on to the DVR.
func (s *Server) bookmarkToDVR(r *http.Request, id string, sec int) {
	rid, ok := strings.CutPrefix(id, "rec/")
	_, prof := s.profileFor(r)
	if prof.Playback == config.PlaybackKeep || !ok {
		return
	}
	f, found, err := s.cfg.DVR.Recording(r.Context(), rid)
	if err != nil || !found {
		return
	}
	cut := prof.Ads == config.AdsCut && s.adFree(f)
	length := f.Duration
	if cut {
		length = f.AdFree(-1)
	}
	s.playedTo(r, f, cut, float64(sec), length)
}

// playedTo tells the DVR a recording was played to t seconds of length, on the
// clock of the version the TV had: watched near the end, else the position.
func (s *Server) playedTo(r *http.Request, f dvr.Recording, cut bool, t, length float64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	id := string(f.ID)
	if length > 0 && t >= length-max(watchedTail, length*0.03) {
		if err := s.cfg.DVR.SetWatched(ctx, id, true); err != nil {
			s.cfg.Logf("playback: recording %s: %v", id, err)
			return
		}
		// No playback_time/0 after: the DVR takes that as not watched.
		s.cfg.Logf("playback: recording %s played to the end by %s: marked watched", id, s.who(r))
		return
	}
	if t < minResume {
		return
	}
	at := t
	if cut {
		at = f.RecordingTime(t) // the DVR keeps the time in the recording
	}
	if err := s.cfg.DVR.SetPlayback(ctx, id, int(at)); err != nil {
		s.cfg.Logf("playback: recording %s: %v", id, err)
		return
	}
	s.cfg.Logf("playback: recording %s left at %s by %s", id, duration(at), s.who(r))
}

// rangeStart is the first byte a Range header asks for, 0 for none.
func rangeStart(r *http.Request) int64 {
	v, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes=")
	if !ok {
		return 0
	}
	a, _, _ := strings.Cut(v, "-")
	n, err := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// byteCounter counts what is written through it.
type byteCounter struct {
	http.ResponseWriter
	n int64
}

func (c *byteCounter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func (c *byteCounter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *byteCounter) Flush() { http.NewResponseController(c.ResponseWriter).Flush() }
