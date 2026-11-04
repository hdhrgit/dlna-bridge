package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"dlna/internal/config"
	"dlna/internal/cut"
	"dlna/internal/dvr"
)

// adFree says whether a recording has breaks to cut out: only once the DVR has
// finished it and marked them.
func (s *Server) adFree(f dvr.Recording) bool {
	return f.Completed && f.Processed && f.FileSize > 0 && len(f.Commercials) >= 2
}

// plans keeps each recording's cut list, which takes a few dozen range reads
// to work out.
type plans struct {
	mu sync.Mutex
	m  map[string]*planEntry
}

type planEntry struct {
	once sync.Once
	plan *cut.Plan
	err  error
}

func (p *plans) get(ctx context.Context, f dvr.Recording, file *dvr.File) (*cut.Plan, error) {
	key := fmt.Sprintf("%s/%d/%v", f.ID, f.FileSize, f.Commercials)
	p.mu.Lock()
	if p.m == nil {
		p.m = map[string]*planEntry{}
	}
	e := p.m[key]
	if e == nil {
		e = &planEntry{}
		p.m[key] = e
	}
	p.mu.Unlock()
	e.once.Do(func() {
		// Not the request's context: a TV that hangs up should not leave a
		// failed plan behind for the next try.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		e.plan, e.err = cut.Make(ctx, file, f.Commercials)
	})
	if e.err != nil {
		p.mu.Lock()
		delete(p.m, key) // try again next time
		p.mu.Unlock()
	}
	return e.plan, e.err
}

// streamAdFree plays a recording without its breaks: seekable like the
// original, or converted if the client's profile says so. ?raw=1 is the
// seekable one whatever the profile, for the converter to read.
func (s *Server) streamAdFree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("raw") == "" {
		if name, prof := s.profileFor(r); prof.Mode == config.ModeTranscode {
			s.transcode(w, r, prof.Transcode, name, "recording "+id, s.cfg.SelfURL+"/stream/adfree/"+url.PathEscape(id)+"?raw=1", false)
			return
		}
	}
	s.adFreeRecording(w, r, id)
}

// adFreeRecording serves a recording without its breaks. If the cut list
// cannot be made it serves the whole recording.
func (s *Server) adFreeRecording(w http.ResponseWriter, r *http.Request, id string) {
	f, found, err := s.cfg.DVR.Recording(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	file := s.cfg.DVR.File(f)
	started := time.Now()
	plan, err := s.plans.get(r.Context(), f, file)
	if err != nil {
		s.cfg.Logf("adfree: recording %s: %v; serving it whole", id, err)
		s.proxy(w, r, dvr.RecordingPath(id), recFeatures)
		return
	}
	if d := time.Since(started); d > 100*time.Millisecond {
		s.cfg.Logf("adfree: recording %s: %d parts, %d of %d bytes, planned in %s", id, len(plan.Parts), plan.Size, f.FileSize, d.Round(time.Millisecond))
	}
	cf := plan.Open(r.Context(), file.Open)
	defer cf.Close()
	w.Header().Set("Content-Type", videoMIME)
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	w.Header().Set("ContentFeatures.DLNA.ORG", recFeatures)
	http.ServeContent(w, r, "", time.Time{}, cf)
}
