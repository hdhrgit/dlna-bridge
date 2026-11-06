package server

import (
	"container/list"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
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
// hundred reads to work out. It holds at most maxPlans in memory, the least
// recently used going first, and a recording's plan is dropped when its size or
// its marked breaks change, so a library that keeps changing does not fill it.
// With a directory, plans are also kept there, so that a restart does not work
// them out again.
type plans struct {
	mu  sync.Mutex
	m   map[string]*list.Element // by recording id
	lru *list.List               // of *planEntry, most recently used first
	// build makes a plan; cut.Make unless a test says otherwise.
	build func(context.Context, cut.Source, []float64) (*cut.Plan, error)
	dir   string
	logf  func(format string, a ...any)
}

const planVersion = 2 // changes when plans made earlier should not be used again: 2, MPEG-2 cut on I pictures

var (
	maxPlans     = 32
	maxPlanFiles = 200
)

type planEntry struct {
	id, key string
	once    sync.Once
	plan    *cut.Plan
	err     error
}

func (p *plans) log(format string, a ...any) {
	if p.logf != nil {
		p.logf(format, a...)
	}
}

func (p *plans) get(ctx context.Context, f dvr.Recording, file *dvr.File) (*cut.Plan, error) {
	id := string(f.ID)
	key := fmt.Sprintf("v%d/%d/%v", planVersion, f.FileSize, f.Commercials)
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
		if plan := p.load(id, key, f.FileSize); plan != nil {
			e.plan = plan
			return
		}
		// Not the request's context: a TV that hangs up should not leave a
		// failed plan behind for the next try.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		build := p.build
		if build == nil {
			build = cut.Make
		}
		started := time.Now()
		e.plan, e.err = build(ctx, file, f.Commercials)
		if e.err != nil {
			p.log("adfree: recording %s: %v; serving it whole", id, e.err)
			return
		}
		p.log("adfree: recording %s: %d parts, %d of %d bytes, planned in %s", id, len(e.plan.Parts), e.plan.Size, f.FileSize, time.Since(started).Round(time.Millisecond))
		p.save(id, key, e.plan)
	})
	// A plan that failed because of a network error is tried again next time; one
	// that failed because the recording's timestamps are no good will fail again.
	if e.err != nil && !errors.Is(e.err, cut.ErrTimeline) {
		p.mu.Lock()
		if cur := p.m[id]; cur == el {
			p.lru.Remove(el)
			delete(p.m, id)
		}
		p.mu.Unlock()
	}
	return e.plan, e.err
}

// planFile is a plan as kept on disk.
type planFile struct {
	Key  string    `json:"key"`
	Plan *cut.Plan `json:"plan"`
}

func (p *plans) path(id string) string {
	return filepath.Join(p.dir, fmt.Sprintf("%x.json", sha1.Sum([]byte(id))))
}

// load reads a plan from disk, if there is one for this recording as it is now.
func (p *plans) load(id, key string, fileSize int64) *cut.Plan {
	if p.dir == "" {
		return nil
	}
	b, err := os.ReadFile(p.path(id))
	if err != nil {
		return nil
	}
	var pf planFile
	if err := json.Unmarshal(b, &pf); err != nil || pf.Key != key || pf.Plan == nil || !pf.Plan.Valid(fileSize) {
		os.Remove(p.path(id)) // for another version of the recording, or damaged
		return nil
	}
	return pf.Plan
}

func (p *plans) save(id, key string, plan *cut.Plan) {
	if p.dir == "" {
		return
	}
	b, err := json.Marshal(planFile{key, plan})
	if err == nil {
		err = os.MkdirAll(p.dir, 0o755)
	}
	if err == nil {
		tmp := p.path(id) + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, p.path(id))
		}
	}
	if err != nil {
		p.log("adfree: keeping the plan of recording %s: %v", id, err)
		return
	}
	p.prune()
}

// prune leaves the newest maxPlanFiles files, so the directory does not grow
// with every recording ever played.
func (p *plans) prune() {
	entries, err := os.ReadDir(p.dir)
	if err != nil || len(entries) <= maxPlanFiles {
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	var files []file
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			files = append(files, file{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[min(maxPlanFiles, len(files)):] {
		os.Remove(filepath.Join(p.dir, f.name))
	}
}

// streamAdFree plays a recording without its breaks: seekable like the
// original, or converted if the client's profile says so. ?raw=1 is the
// seekable one whatever the profile, for the converter to read.
func (s *Server) streamAdFree(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("raw") == "" {
		if name, prof := s.profileFor(r); prof.Mode == config.ModeTranscode {
			var length float64 // without the breaks, as the seconds of a seek are
			if f, found, err := s.cfg.DVR.Recording(r.Context(), id); err == nil && found {
				length = f.AdFree(-1)
			}
			s.transcode(w, r, prof.Transcode, name, "recording "+id, s.cfg.SelfURL+"/stream/adfree/"+url.PathEscape(id)+"?raw=1", false, length)
			return
		}
	}
	name, prof := s.profileFor(r)
	tw, done := s.tracked(w, r, "recording", name, s.whatRecording(r, id)+" (ads cut)")
	defer done()
	if prof.Seek == config.SeekTime && r.URL.Query().Get("raw") == "" {
		s.adFreeByTime(tw, r, id)
		return
	}
	s.adFreeRecording(tw, r, id)
}

// adFreeByTime serves a recording without its breaks to a client that seeks by
// time: from the start, or from the time in TimeSeekRange.dlna.org, with no
// length. The first bytes go out after a few reads of the recording; each
// break is found just before it is reached.
func (s *Server) adFreeByTime(w http.ResponseWriter, r *http.Request, id string) {
	f, found, err := s.cfg.DVR.Recording(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if rg := r.Header.Get("Range"); rg != "" && rg != "bytes=0-" {
		// It has no byte offsets; DLNA's answer to a seek that is not offered.
		http.Error(w, "this stream seeks by time (TimeSeekRange.dlna.org), not by bytes", http.StatusNotAcceptable)
		return
	}
	total := f.AdFree(-1)
	start := 0.0
	seek := r.Header.Get("TimeSeekRange.dlna.org")
	if seek != "" {
		var ok bool
		if start, ok = parseNPT(seek); !ok {
			http.Error(w, "bad TimeSeekRange.dlna.org", http.StatusBadRequest)
			return
		}
		if total > 0 && start >= total {
			http.Error(w, "seek past the end", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	w.Header().Set("Content-Type", videoMIME)
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	w.Header().Set("ContentFeatures.DLNA.ORG", timeFeatures)
	if seek != "" {
		w.Header().Set("TimeSeekRange.dlna.org", "npt="+duration(start)+"-"+duration(total)+"/"+duration(total))
	}
	if r.Method == http.MethodHead {
		return
	}
	file := s.cfg.DVR.File(f)
	logf := func(format string, a ...any) { s.cfg.Logf("recording %s: "+format, append([]any{id}, a...)...) }
	st, err := cut.NewStream(r.Context(), file, f.Commercials, f.RecordingTime(start), file.Open, logf)
	if err != nil {
		if r.Context().Err() == nil {
			logf("cannot start at %s: %v", duration(start), err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}
	defer st.Close()
	w.WriteHeader(http.StatusOK)
	copyFlushing(w, st)
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
	plan, err := s.plans.get(r.Context(), f, file)
	if err != nil {
		s.proxy(w, r, dvr.RecordingPath(id), recFeatures) // the plans log why
		return
	}
	cf := plan.Open(r.Context(), file.Open)
	defer cf.Close()
	w.Header().Set("Content-Type", videoMIME)
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	w.Header().Set("ContentFeatures.DLNA.ORG", recFeatures)
	bc, began := &byteCounter{ResponseWriter: w}, time.Now()
	http.ServeContent(bc, r, "", time.Time{}, cf)
	s.streamPosition(r, f, true, rangeStart(r), bc.n, plan.Size, began)
}
