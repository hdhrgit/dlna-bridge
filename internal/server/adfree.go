package server

import (
	"container/list"
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

// plans keeps the cut list of the recordings played lately, which takes a few
// dozen range reads to work out. It holds at most maxPlans, the least recently
// used going first, and a recording's plan is dropped when its size or its
// marked breaks change, so a library that keeps changing does not fill it.
type plans struct {
	mu  sync.Mutex
	m   map[string]*list.Element // by recording id
	lru *list.List               // of *planEntry, most recently used first
	// build makes a plan; cut.Make unless a test says otherwise.
	build func(context.Context, cut.Source, []float64) (*cut.Plan, error)
}

const maxPlans = 32

type planEntry struct {
	id, key string
	once    sync.Once
	plan    *cut.Plan
	err     error
}

func (p *plans) get(ctx context.Context, f dvr.Recording, file *dvr.File) (*cut.Plan, error) {
	id := string(f.ID)
	key := fmt.Sprintf("%d/%v", f.FileSize, f.Commercials)
	p.mu.Lock()
	if p.m == nil {
		p.m, p.lru = map[string]*list.Element{}, list.New()
	}
	el := p.m[id]
	if el != nil && el.Value.(*planEntry).key != key {
		p.lru.Remove(el) // the recording is not what the plan was made for
		el = nil
	}
	if el == nil {
		el = p.lru.PushFront(&planEntry{id: id, key: key})
		p.m[id] = el
		for p.lru.Len() > maxPlans {
			old := p.lru.Remove(p.lru.Back()).(*planEntry)
			delete(p.m, old.id)
		}
	} else {
		p.lru.MoveToFront(el)
	}
	e := el.Value.(*planEntry)
	p.mu.Unlock()
	e.once.Do(func() {
		// Not the request's context: a TV that hangs up should not leave a
		// failed plan behind for the next try.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		build := p.build
		if build == nil {
			build = cut.Make
		}
		e.plan, e.err = build(ctx, file, f.Commercials)
	})
	if e.err != nil {
		p.mu.Lock()
		if cur := p.m[id]; cur == el { // try again next time
			p.lru.Remove(el)
			delete(p.m, id)
		}
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
	name, _ := s.profileFor(r)
	tw, done := s.tracked(w, r, "recording", name, s.whatRecording(r, id)+" (ads cut)")
	defer done()
	s.adFreeRecording(tw, r, id)
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
