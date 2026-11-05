package server

import (
	"context"
	"time"

	"dlna/internal/dvr"
)

// registration is how the last try at registering with the DVR went.
type registration struct {
	Name  string    `json:"name"`
	ID    string    `json:"id,omitempty"`
	At    time.Time `json:"at,omitempty"` // of the last success
	Error string    `json:"error,omitempty"`
	// IP is the address the DVR sees the bridge at, from its client list, and
	// DVRName what it calls that client there: another name when another
	// client reaches the DVR from the same address, which the DVR cannot tell
	// apart from the bridge.
	IP      string `json:"ip,omitempty"`
	DVRName string `json:"dvr_name,omitempty"`
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
		var entry dvr.ClientEntry
		if err == nil {
			// Not fatal: without it the status page shows all the DVR's activity.
			if e, found, ierr := s.cfg.DVR.ClientInfo(ctx1, id); ierr == nil && found {
				entry = e
			}
		}
		cancel()
		s.regMu.Lock()
		first := s.reg.ID == "" && s.reg.Error == ""
		failedBefore := s.reg.Error != ""
		s.reg.Name = s.cfg.ClientName
		if err != nil {
			s.reg.Error = err.Error()
		} else {
			s.reg.ID, s.reg.At, s.reg.Error = id, time.Now(), ""
			s.reg.IP, s.reg.DVRName = entry.LocalIP, entry.Hostname
		}
		s.regMu.Unlock()
		switch {
		case err != nil && (first || !failedBefore):
			s.cfg.Logf("registering with the DVR as %q: %v; trying again in %s", s.cfg.ClientName, err, wait)
		case err == nil && (first || failedBefore):
			s.cfg.Logf("registered with the DVR as %q (client %s)", s.cfg.ClientName, id)
			if entry.Hostname != "" && entry.Hostname != s.cfg.ClientName {
				s.cfg.Logf("the DVR calls client %s %q: another client reaches it from the same address, %s, and it cannot tell them apart; point -dvr at an address of the DVR that only the bridge uses", id, entry.Hostname, entry.LocalIP)
			}
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
