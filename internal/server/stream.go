package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"dlna/internal/config"
	"dlna/internal/dvr"
)

// streamLive plays a channel to a client, as the client's profile says: passed
// through from the DVR, or converted.
func (s *Server) streamLive(w http.ResponseWriter, r *http.Request) {
	number, dev := r.PathValue("number"), r.URL.Query().Get("dev")
	name, prof := s.profileFor(r)
	if prof.Mode != config.ModeTranscode {
		s.proxy(w, r, dvr.LivePath(dev, number), liveFeatures)
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
	t := prof.Transcode
	input := s.cfg.DVR.Base.String() + dvr.LivePath(dev, number)
	if t.PreScale {
		// The DVR scales the picture down first, which costs it less than
		// sending the full one for us to decode.
		input = s.cfg.DVR.SmallStreamURL(dev, number, evenCeil(t.MaxHeight), t.VideoKbps*2+t.AudioKbps)
	}
	s.transcode(w, r, t, name, number+" "+ch.Name, input)
}

func (s *Server) streamRecording(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name, prof := s.profileFor(r)
	if prof.Mode != config.ModeTranscode {
		s.proxy(w, r, dvr.RecordingPath(id), recFeatures)
		return
	}
	s.transcode(w, r, prof.Transcode, name, "recording "+id, s.cfg.DVR.Base.String()+dvr.RecordingPath(id))
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, dvrPath, features string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, s.cfg.DVR.Base.String()+dvrPath, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" {
		req.Header.Set("Range", rg)
	}
	// No client timeout: a stream runs as long as the TV watches.
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("Content-Type", videoMIME)
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	w.Header().Set("ContentFeatures.DLNA.ORG", features)
	w.WriteHeader(resp.StatusCode)
	copyFlushing(w, resp.Body)
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
