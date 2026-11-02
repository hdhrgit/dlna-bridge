package server

import (
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"dlna/internal/config"
)

// profileFor finds how to serve the request's client. ?profile=<client name>
// picks one by name, for devices that cannot be told apart by address.
func (s *Server) profileFor(r *http.Request) (string, config.Profile) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ua := r.Header.Get("User-Agent")
	name, prof := s.cfg.Settings.Match(net.ParseIP(host), ua, r.URL.Query().Get("profile"))
	s.clients.note(host, ua, name)
	return name, prof
}

// seenClient is a device that has talked to the server, for the settings page
// to offer when making a client.
type seenClient struct {
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Profile   string    `json:"profile"` // the client it matched, or empty for the default
	Last      time.Time `json:"last"`
	Requests  int       `json:"requests"`
}

type clientLog struct {
	mu sync.Mutex
	m  map[[2]string]*seenClient
}

const maxSeen = 100

func (c *clientLog) note(ip, ua, profile string) {
	if ip == "127.0.0.1" || ip == "::1" {
		return // this server's own converter
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[[2]string]*seenClient{}
	}
	k := [2]string{ip, ua}
	e := c.m[k]
	if e == nil {
		if len(c.m) >= maxSeen {
			var oldest [2]string
			var t time.Time
			for k, e := range c.m {
				if t.IsZero() || e.Last.Before(t) {
					oldest, t = k, e.Last
				}
			}
			delete(c.m, oldest)
		}
		e = &seenClient{IP: ip, UserAgent: ua}
		c.m[k] = e
	}
	e.Profile, e.Last = profile, time.Now()
	e.Requests++
}

func (c *clientLog) list() []seenClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]seenClient, 0, len(c.m))
	for _, e := range c.m {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// job is a conversion that is running.
type job struct {
	ID     int       `json:"id"`
	Client string    `json:"client"`
	IP     string    `json:"ip"`
	What   string    `json:"what"`
	Since  time.Time `json:"since"`
}

type jobs struct {
	mu   sync.Mutex
	next int
	m    map[int]job
}

// start adds a job unless limit are already running.
func (j *jobs) start(limit int, client, ip, what string) (int, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.m) >= limit {
		return 0, false
	}
	if j.m == nil {
		j.m = map[int]job{}
	}
	j.next++
	j.m[j.next] = job{ID: j.next, Client: client, IP: ip, What: what, Since: time.Now()}
	return j.next, true
}

func (j *jobs) end(id int) {
	j.mu.Lock()
	delete(j.m, id)
	j.mu.Unlock()
}

func (j *jobs) list() []job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]job, 0, len(j.m))
	for _, e := range j.m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}
