package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"dlna/internal/cut"
	"dlna/internal/dvr"
)

// adFree says whether a recording is served with its breaks cut out: only
// once the DVR has finished it and marked them.
func (s *Server) adFree(f dvr.Recording) bool {
	return !s.cfg.KeepAds && f.Completed && f.Processed && f.FileSize > 0 && len(f.Commercials) >= 2
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

// adFreeRecording serves a recording without its breaks, seekable like the
// original. If the cut list cannot be made it serves the whole recording.
func (s *Server) adFreeRecording(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
