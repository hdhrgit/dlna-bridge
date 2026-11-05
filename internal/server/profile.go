package server

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dlna/internal/config"
)

// profileFor finds how to serve the request's client. ?profile=<client name>
// picks one by name, for devices that cannot be told apart by address.
func (s *Server) profileFor(r *http.Request) (string, config.Profile) {
	host := s.clientIP(r)
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

// job is a stream that is running through the bridge: a conversion, or a
// stream passed through.
type job struct {
	ID        int
	Kind      string // live, recording, or converted
	Client    string
	IP        string
	UserAgent string
	What      string
	Since     time.Time
	bytes     *atomic.Int64
}

// jobView is what the page shows: one row for each client and thing watched,
// since a TV opens a dozen connections to one recording.
type jobView struct {
	Kind      string    `json:"kind"`
	Client    string    `json:"client"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	What      string    `json:"what"`
	Since     time.Time `json:"since"`
	Bytes     int64     `json:"bytes"`
	Conns     int       `json:"conns"`
}

type jobs struct {
	mu   sync.Mutex
	next int
	m    map[int]*job
}

func (j *jobs) add(kind, client, ip, ua, what string) int {
	if j.m == nil {
		j.m = map[int]*job{}
	}
	j.next++
	j.m[j.next] = &job{ID: j.next, Kind: kind, Client: client, IP: ip, UserAgent: ua, What: what, Since: time.Now(), bytes: new(atomic.Int64)}
	return j.next
}

// start adds a conversion unless limit are already running.
func (j *jobs) start(limit int, client, ip, ua, what string) (int, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	for _, e := range j.m {
		if e.Kind == "converted" {
			n++
		}
	}
	if n >= limit {
		return 0, false
	}
	return j.add("converted", client, ip, ua, what), true
}

// track adds a stream passed through, which has no limit.
func (j *jobs) track(kind, client, ip, ua, what string) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.add(kind, client, ip, ua, what)
}

// counter is where to add the bytes sent for a job.
func (j *jobs) counter(id int) *atomic.Int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e := j.m[id]; e != nil {
		return e.bytes
	}
	return new(atomic.Int64)
}

func (j *jobs) end(id int) {
	j.mu.Lock()
	delete(j.m, id)
	j.mu.Unlock()
}

func (j *jobs) list() []jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	type key struct{ kind, ip, what string }
	at := map[key]int{}
	out := []jobView{}
	ids := make([]int, 0, len(j.m))
	for id := range j.m {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		e := j.m[id]
		k := key{e.Kind, e.IP, e.What}
		if i, ok := at[k]; ok {
			out[i].Bytes += e.bytes.Load()
			out[i].Conns++
			if e.Since.Before(out[i].Since) {
				out[i].Since = e.Since
			}
			continue
		}
		at[k] = len(out)
		out = append(out, jobView{Kind: e.Kind, Client: e.Client, IP: e.IP, UserAgent: e.UserAgent, What: e.What, Since: e.Since, Bytes: e.bytes.Load(), Conns: 1})
	}
	return out
}

// countWriter counts the bytes written to a response.
type countWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n.Add(int64(n))
	return n, err
}

func (c *countWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

func (c *countWriter) FlushError() error { return http.NewResponseController(c.ResponseWriter).Flush() }

// tracked lists a stream passed through while it runs, with the bytes sent.
// Requests for headers, and the bridge's own reads, are not listed.
func (s *Server) tracked(w http.ResponseWriter, r *http.Request, kind, client, what string) (http.ResponseWriter, func()) {
	ip := s.clientIP(r)
	if r.Method == http.MethodHead || ip == "127.0.0.1" || ip == "::1" {
		return w, func() {}
	}
	id := s.jobs.track(kind, client, ip, r.Header.Get("User-Agent"), what)
	return &countWriter{w, s.jobs.counter(id)}, func() { s.jobs.end(id) }
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ParseProxies reads a comma separated list of addresses and ranges.
func ParseProxies(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			ip := net.ParseIP(f)
			if ip == nil {
				return nil, fmt.Errorf("%q is not an address or range", f)
			}
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(f)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or range", f)
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *Server) trusted(ip net.IP) bool {
	for _, n := range s.cfg.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the address of the device making the request. Behind a proxy
// listed in TrustedProxies it is the nearest address in X-Forwarded-For that is
// not itself a trusted proxy, or X-Real-IP; from anyone else the headers are
// ignored, since anyone could send them.
func (s *Server) clientIP(r *http.Request) string {
	remote := remoteIP(r)
	if !s.trusted(net.ParseIP(remote)) {
		return remote
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break // not an address: take nothing from the header
		}
		if !s.trusted(ip) {
			return ip.String()
		}
	}
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
		return ip.String()
	}
	return remote
}

// hostOf is the host and port the client used to reach the server: the Host
// header, or what a trusted proxy says it was asked for.
func (s *Server) hostOf(r *http.Request) string {
	if s.trusted(net.ParseIP(remoteIP(r))) {
		if h, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); strings.TrimSpace(h) != "" {
			return strings.TrimSpace(h)
		}
	}
	return r.Host
}

// whatLive names a channel for the page.
func (s *Server) whatLive(r *http.Request, number string) string {
	if ch, ok, _ := s.cfg.DVR.Channel(r.Context(), number); ok {
		return ch.Number + " " + ch.Name
	}
	return number
}

// whatRecording names a recording for the page.
func (s *Server) whatRecording(r *http.Request, id string) string {
	if f, ok, _ := s.cfg.DVR.Recording(r.Context(), id); ok {
		return f.Airing.Title + " " + recordingTitle(f)
	}
	return "recording " + id
}
