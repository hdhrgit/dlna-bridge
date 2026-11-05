package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"dlna/internal/dvr"
)

// TVs keep the lists they browsed and ask again only when told that something
// changed: the ContentDirectory event, which they subscribe to. WatchChanges
// follows the DVR's own events and, when a recording starts, finishes, is
// marked or is deleted, raises the SystemUpdateID and tells every subscribed
// TV.

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
	var told []string
	defer func() { s.cfg.Logf("recordings changed: update %d sent to %d TVs %v", id, len(told), told) }()
	for sid, sub := range s.subs {
		if now.After(sub.expires) {
			delete(s.subs, sid)
			continue
		}
		s.notify(sub.cb, sid, sub.seq, props)
		sub.seq++
		told = append(told, sub.cb)
	}
}

// WatchChanges follows the DVR's event stream and calls changed when a
// recording is created, updated (finished, marked) or deleted, or a show
// changes. A burst of events, as one recording finishing sends, makes one
// change. When the stream drops, as when the DVR restarts, it looks at the
// DVR's newest recordings every watchEvery instead, and connects again with a
// growing wait; on each connect it compares them once, for what it missed.
func (s *Server) WatchChanges(ctx context.Context) {
	// last is the newest recordings as of the last change event: what a
	// reconnect compares with, for changes made while the stream was down.
	var mu sync.Mutex
	last := s.newestSig(ctx)
	changedSince := func() {
		sig := s.newestSig(ctx)
		mu.Lock()
		differs := sig != "" && last != "" && sig != last
		if sig != "" {
			last = sig
		}
		mu.Unlock()
		if differs {
			s.cfg.DVR.ForgetRecordings()
			s.changed()
		}
	}
	wait := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		var pending *time.Timer
		err := s.cfg.DVR.Events(ctx, func(e dvr.Event) {
			switch e.Type {
			case "hello":
				s.cfg.Logf("DVR events: connected (DVR %s)", e.Version)
				changedSince()
			case "files.created", "files.updated", "files.deleted", "groups.updated":
				mu.Lock()
				defer mu.Unlock()
				if pending == nil {
					pending = time.AfterFunc(eventSettle, func() {
						mu.Lock()
						pending = nil
						mu.Unlock()
						s.cfg.DVR.ForgetRecordings()
						s.changed()
						if sig := s.newestSig(ctx); sig != "" {
							mu.Lock()
							last = sig
							mu.Unlock()
						}
					})
				}
			}
		})
		mu.Lock()
		if pending != nil {
			pending.Stop()
		}
		mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			wait = time.Second // it was up a while: this is a new failure
		}
		s.cfg.Logf("DVR events: %v; looking at the newest recordings every %s, connecting again in %s", err, s.watchEvery, wait)
		for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(s.watchEvery, time.Until(deadline))):
			}
			changedSince()
		}
		wait = min(wait*2, time.Minute)
	}
}

// eventSettle is how long WatchChanges waits after a change event for the
// others that come with it.
const eventSettle = 2 * time.Second

// newestSig sums up the DVR's newest recordings, "" when it cannot be asked.
func (s *Server) newestSig(ctx context.Context) string {
	newest, err := s.cfg.DVR.Newest(ctx, planAheadLook)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, n := range newest {
		fmt.Fprintf(&b, "%s %v %v %d;", n.ID, n.Completed, n.Processed, len(n.Commercials))
	}
	return b.String()
}
