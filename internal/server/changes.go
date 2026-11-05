package server

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TVs keep the lists they browsed and ask again only when told that something
// changed: the ContentDirectory event, which they subscribe to. WatchChanges
// looks at the DVR's newest recordings and, when one starts, finishes or is
// marked, raises the SystemUpdateID and tells every subscribed TV.

const (
	watchEvery = time.Minute
	subTimeout = 30 * time.Minute // what SUBSCRIBE grants; a TV renews before
	maxSubs    = 64
)

type subscription struct {
	cb      string
	seq     int64 // of the next event; the first, on subscribing, is 0
	expires time.Time
}

func (s *Server) subscribe(sid, cb string) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	if s.subs == nil {
		s.subs = map[string]*subscription{}
	}
	now := time.Now()
	for k, v := range s.subs {
		if now.After(v.expires) {
			delete(s.subs, k)
		}
	}
	if len(s.subs) >= maxSubs {
		return // a TV that does not get events still browses
	}
	s.subs[sid] = &subscription{cb: cb, seq: 1, expires: now.Add(subTimeout)}
}

// renew extends a subscription; false when there is no such one.
func (s *Server) renew(sid string) bool {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	sub := s.subs[sid]
	if sub == nil || time.Now().After(sub.expires) {
		return false
	}
	sub.expires = time.Now().Add(subTimeout)
	return true
}

func (s *Server) unsubscribe(sid string) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	delete(s.subs, sid)
}

// changed raises the SystemUpdateID and tells the subscribed TVs, naming the
// folders whose contents changed.
func (s *Server) changed() {
	id := max(s.updateID.Load()+1, time.Now().Unix()/60%1000000)
	s.updateID.Store(id)
	var ids []string
	for _, f := range []string{idRoot, idRecent, idRecordings, idMovies, idRecNow} {
		ids = append(ids, f, fmt.Sprint(id))
	}
	props := [][2]string{{"SystemUpdateID", fmt.Sprint(id)}, {"ContainerUpdateIDs", strings.Join(ids, ",")}}
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	now := time.Now()
	for sid, sub := range s.subs {
		if now.After(sub.expires) {
			delete(s.subs, sid)
			continue
		}
		s.notify(sub.cb, sid, sub.seq, props)
		sub.seq++
	}
}

// WatchChanges looks at the DVR's newest recordings every watchEvery, one
// short request, and calls changed when they are not what they were: a
// recording started, finished, was marked or was deleted among them.
func (s *Server) WatchChanges(ctx context.Context) {
	last := ""
	for {
		if newest, err := s.cfg.DVR.Newest(ctx, planAheadLook); err == nil {
			var b strings.Builder
			for _, n := range newest {
				fmt.Fprintf(&b, "%s %v %v %d;", n.ID, n.Completed, n.Processed, len(n.Commercials))
			}
			if sig := b.String(); last != "" && sig != last {
				s.changed()
			}
			last = b.String()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.watchEvery):
		}
	}
}
