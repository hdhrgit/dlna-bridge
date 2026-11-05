package server

import (
	"context"
	"time"
)

// registration is how the last try at registering with the DVR went.
type registration struct {
	Name  string    `json:"name"`
	ID    string    `json:"id,omitempty"`
	At    time.Time `json:"at,omitempty"` // of the last success
	Error string    `json:"error,omitempty"`
}

// KeepRegistered registers this program with the DVR under Config.ClientName,
// again every half hour, in case the DVR lost the entry, and after a failure
// with a longer wait each time, until ctx ends. The DVR names what this
// address watches after the client, so that its activity list says "from DLNA
// Bridge" where it would say the bridge's address.
func (s *Server) KeepRegistered(ctx context.Context) {
	if s.cfg.ClientName == "" {
		return
	}
	wait := s.regRetryMin
	for {
		ctx1, cancel := context.WithTimeout(ctx, 20*time.Second)
		id, err := s.cfg.DVR.Register(ctx1, s.cfg.ClientName, s.cfg.UUID)
		cancel()
		s.regMu.Lock()
		first := s.reg.ID == "" && s.reg.Error == ""
		failedBefore := s.reg.Error != ""
		s.reg.Name = s.cfg.ClientName
		if err != nil {
			s.reg.Error = err.Error()
		} else {
			s.reg.ID, s.reg.At, s.reg.Error = id, time.Now(), ""
		}
		s.regMu.Unlock()
		switch {
		case err != nil && (first || !failedBefore):
			s.cfg.Logf("registering with the DVR as %q: %v; trying again in %s", s.cfg.ClientName, err, wait)
		case err == nil && (first || failedBefore):
			s.cfg.Logf("registered with the DVR as %q (client %s)", s.cfg.ClientName, id)
		}
		next := s.regEvery
		if err != nil {
			next = wait
			wait = min(wait*2, s.regRetryMax)
		} else {
			wait = s.regRetryMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

func (s *Server) registration() *registration {
	if s.cfg.ClientName == "" {
		return nil
	}
	s.regMu.Lock()
	defer s.regMu.Unlock()
	r := s.reg
	if r.Name == "" {
		r.Name = s.cfg.ClientName
	}
	return &r
}
