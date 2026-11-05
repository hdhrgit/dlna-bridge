package server

import (
	"context"
	"fmt"
	"time"

	"dlna/internal/config"
)

// How often the DVR's newest recordings are looked at, and how many.
const (
	planAheadEvery = 2 * time.Minute
	planAheadLook  = 25
)

// PlanAhead works out the cuts of each new recording once the DVR has marked
// its breaks, so that the first play starts without waiting for them. Only
// recordings that are new since the start are planned, not the library: it
// looks at the DVR's newest few, one short request every planAheadEvery, and
// plans one at a time. It does nothing without a plans directory, or while no
// client has its commercials cut.
func (s *Server) PlanAhead(ctx context.Context) {
	if s.cfg.PlansDir == "" {
		return
	}
	seen := map[string]bool{}
	first := true
	for {
		s.planNewest(ctx, seen, first)
		first = false
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.planEvery):
		}
	}
}

// planNewest plans the newest recordings not seen before. The first time it
// only notes them: they were there before the start.
func (s *Server) planNewest(ctx context.Context, seen map[string]bool, first bool) {
	newest, err := s.cfg.DVR.Newest(ctx, planAheadLook)
	if err != nil {
		if !first {
			s.cfg.Logf("plan ahead: %v", err)
		}
		return
	}
	for _, n := range newest {
		if !n.Completed || !n.Processed || len(n.Commercials) < 2 {
			continue
		}
		key := fmt.Sprint(n.ID, n.Commercials)
		if seen[key] {
			continue
		}
		seen[key] = true
		if first || !s.cutsAds() {
			continue
		}
		f, err := s.cfg.DVR.RecordingByID(ctx, n.ID)
		if err != nil {
			s.cfg.Logf("plan ahead: recording %s: %v", n.ID, err)
			continue
		}
		if !s.adFree(f) {
			continue
		}
		s.cfg.Logf("plan ahead: recording %s, %s", f.ID, f.Airing.Title)
		s.plans.get(ctx, f, s.cfg.DVR.File(f)) // logs how it went
		if ctx.Err() != nil {
			return
		}
	}
}

// cutsAds says whether any client, or the default, has commercials cut.
func (s *Server) cutsAds() bool {
	c := s.cfg.Settings.Get()
	if c.Default.Ads == config.AdsCut {
		return true
	}
	for _, cl := range c.Clients {
		if cl.Profile.Ads == config.AdsCut {
			return true
		}
	}
	return false
}
