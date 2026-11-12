package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
)

// streamLive plays a channel to a client, as the client's profile says: passed
// through from the DVR, or converted.
func (s *Server) streamLive(w http.ResponseWriter, r *http.Request) {
	number, dev := r.PathValue("number"), r.URL.Query().Get("dev")
	name, prof := s.profileFor(r)
	if prof.Mode != config.ModeTranscode {
		obj := "live/" + number
		if dev != "" {
			obj += "?" + dev
		}
		tw, done := s.tracked(w, r, "live", name, s.whatLive(r, number), obj)
		defer done()
		s.proxy(tw, r, dvr.LivePath(dev, number), liveFeatures)
		return
	}
	ch, found, err := s.cfg.DVR.Channel(r.Context(), number)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	s.transcode(w, r, prof.Transcode, name, number+" "+ch.Name, "", &feedReq{dev: dev, number: number}, true, 0)
}

func (s *Server) streamRecording(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name, prof := s.profileFor(r)
	if prof.Mode != config.ModeTranscode {
		tw, done := s.tracked(w, r, "recording", name, s.whatRecording(r, id), "rec/"+id)
		defer done()
		features := recFeatures
		if f, found, err := s.cfg.DVR.Recording(r.Context(), id); err == nil && found && !f.Completed {
			features = growingFeatures
		}
		bc, began := &byteCounter{ResponseWriter: tw}, time.Now()
		s.proxy(bc, r, dvr.RecordingPath(id), features)
		// The TV has usually hung up by now, which ends r's context.
		if f, found, err := s.cfg.DVR.Recording(context.WithoutCancel(r.Context()), id); err == nil && found {
			s.streamPosition(r, f, false, rangeStart(r), bc.n, f.FileSize, began)
		}
		return
	}
	var length float64 // for the answer to a time seek
	if f, found, err := s.cfg.DVR.Recording(r.Context(), id); err == nil && found {
		length = f.Duration
	}
	s.transcode(w, r, prof.Transcode, name, "recording "+id, s.cfg.DVR.Base.String()+dvr.RecordingPath(id), nil, false, length)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, dvrPath, features string) {
	method := r.Method
	rg := r.Header.Get("Range")
	if method == http.MethodHead {
		// The DVR refuses HEAD on its streams, and a TV that is refused will not
		// play. Answer as the stream would: for a recording ask the DVR for its
		// first byte to learn the length; a live stream has none.
		method = http.MethodGet
		rg = ""
		if features != liveFeatures {
			rg = "bytes=0-0"
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), method, s.cfg.DVR.Base.String()+dvrPath, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.cfg.DVR.SetAgent(req)
	if rg != "" {
		req.Header.Set("Range", rg)
	}
	if r.Method == http.MethodHead && features == liveFeatures {
		s.dlnaHeaders(w, features)
		return
	}
	if features == growingFeatures && r.Method == http.MethodGet && rangeStart(r) > 0 {
		// The DVR does not answer a range past what it has of a recording
		// still being made, until it has it: refuse one past the end now.
		if end, ok := s.growingEnd(r, dvrPath); ok && rangeStart(r) > end {
			s.dlnaHeaders(w, features)
			w.Header().Set("availableSeekRange.dlna.org", "1 bytes=0-"+strconv.FormatInt(end, 10))
			http.Error(w, "not recorded yet", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	// No client timeout: a stream runs as long as the TV watches.
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	s.dlnaHeaders(w, features)
	if r.Method == http.MethodHead {
		// Content-Range is "bytes 0-0/<length>"; the HEAD answer is the whole file.
		if _, total, ok := strings.Cut(resp.Header.Get("Content-Range"), "/"); ok && total != "*" {
			w.Header().Set("Content-Length", total)
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		// A recording still being made has no length yet ("bytes 0-N/*"):
		// it is a stream whose end moves, seekable up to N. A 206 without a
		// range asked for would read as an error (an LG: "716 Resource not
		// found").
		if resp.StatusCode/100 == 2 {
			w.Header().Set("ContentFeatures.DLNA.ORG", growingFeatures)
			w.Header().Set("Accept-Ranges", "bytes")
			availableRange(w, resp)
			return
		}
		w.WriteHeader(resp.StatusCode)
		return
	}
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if features == growingFeatures {
		availableRange(w, resp)
	}
	w.WriteHeader(resp.StatusCode)
	copyFlushing(w, resp.Body)
}

// growingEnd is the last byte the DVR has of a recording still being made.
func (s *Server) growingEnd(r *http.Request, dvrPath string) (int64, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", s.cfg.DVR.Base.String()+dvrPath, nil)
	if err != nil {
		return 0, false
	}
	s.cfg.DVR.SetAgent(req)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return 0, false
	}
	resp.Body.Close()
	rng, total, ok := strings.Cut(strings.TrimPrefix(resp.Header.Get("Content-Range"), "bytes "), "/")
	if !ok || total != "*" {
		return 0, false
	}
	_, e, _ := strings.Cut(rng, "-")
	end, err := strconv.ParseInt(e, 10, 64)
	return end, err == nil
}

// availableRange tells a TV how far into a recording still being made it can
// seek: to the last byte the DVR has, from its "bytes a-N/*".
func availableRange(w http.ResponseWriter, resp *http.Response) {
	cr := resp.Header.Get("Content-Range")
	rng, total, ok := strings.Cut(strings.TrimPrefix(cr, "bytes "), "/")
	if !ok || total != "*" {
		return
	}
	if _, end, ok := strings.Cut(rng, "-"); ok {
		w.Header().Set("availableSeekRange.dlna.org", "1 bytes=0-"+end)
	}
}

func (s *Server) dlnaHeaders(w http.ResponseWriter, features string) {
	w.Header().Set("Content-Type", videoMIME)
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	w.Header().Set("ContentFeatures.DLNA.ORG", features)
}

// copyFlushing copies a stream, flushing as it goes so a live stream does not
// sit in a buffer.
func copyFlushing(w http.ResponseWriter, r io.Reader) {
	f, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if f != nil {
				f.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// Bookmarks remembers where a Samsung TV stopped each recording. With no file
// they live in memory only.
type Bookmarks struct {
	mu   sync.Mutex
	file string
	pos  map[string]int
}

func OpenBookmarks(dir string) (*Bookmarks, error) {
	b := &Bookmarks{pos: map[string]int{}}
	if dir == "" {
		return b, nil
	}
	b.file = filepath.Join(dir, "bookmarks.json")
	data, err := os.ReadFile(b.file)
	if os.IsNotExist(err) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &b.pos); err != nil {
		return nil, fmt.Errorf("%s: %w", b.file, err)
	}
	return b, nil
}

func (b *Bookmarks) Get(id string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pos[id]
}

func (b *Bookmarks) Set(id string, sec int) error {
	if b == nil || id == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if sec <= 0 {
		delete(b.pos, id)
	} else {
		b.pos[id] = sec
	}
	if b.file == "" {
		return nil
	}
	data, _ := json.Marshal(b.pos)
	tmp := b.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, b.file)
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
