package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"dlna/internal/renderer"
)

// ContentDirectory is the service a DLNA media server lists its media with.
const ContentDirectory = "urn:schemas-upnp-org:service:ContentDirectory:1"

// Server is a DLNA media server served in place of the DVR. It is found by an
// SSDP search for a ContentDirectory, taking the answer from its address
// alone, at start and again when it stops answering, since a server can come
// back on another port. Given the URL of its description, that is read
// instead.
type Server struct {
	addr  string // as given: an address or host, or its description's URL
	agent string
	logf  func(string, ...any)

	mu   sync.Mutex
	ip   string // what the description, control URL, streams and pictures must be on
	ctl  string // its ContentDirectory's control URL; empty until found
	name string
	icon string
	caps string // its search capabilities
	err  error  // why it did not answer last, nil when it did
	told string // the last failure to find it that was logged
	back func()
}

// sourceWait is how long the source has to answer an action.
const sourceWait = 5 * time.Second

// NewServer is the server at addr, not yet looked for.
func NewServer(addr, agent string, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{addr: addr, agent: agent, logf: logf, err: errors.New("not found yet")}
}

// IP is the address everything of the server's must be on.
func (d *Server) IP() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ip
}

// Name is its friendlyName, once found.
func (d *Server) Name() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.name
}

// SearchCaps are its search capabilities: empty when it does not search.
func (d *Server) SearchCaps() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.caps
}

// Status says whether it answered last, and if not, why; and its control URL
// and largest icon.
func (d *Server) Status() (ctl, icon string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ctl, d.icon, d.err
}

// Run looks for the server at start and, while it does not answer, again
// every minute. back is called each time it answers again after it did not.
func (d *Server) Run(ctx context.Context, back func()) {
	d.mu.Lock()
	d.back = back
	d.mu.Unlock()
	for {
		d.mu.Lock()
		down := d.err != nil
		d.mu.Unlock()
		if down {
			d.find(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// Call sends an action to its ContentDirectory. A refusal (a SOAP fault) is
// an answer; no answer marks the server down, to be looked for again.
func (d *Server) Call(ctx context.Context, action string, args [][2]string) (map[string]string, error) {
	d.mu.Lock()
	ctl := d.ctl
	d.mu.Unlock()
	if ctl == "" {
		return nil, errors.New("the DLNA server has not been found")
	}
	ctx, cancel := context.WithTimeout(ctx, sourceWait)
	defer cancel()
	v, err := renderer.Call(ctx, ctl, ContentDirectory, action, args)
	var fault *renderer.Fault
	if err != nil && !errors.As(err, &fault) {
		d.mu.Lock()
		if d.err == nil {
			d.logf("source: the DLNA server does not answer: %v", err)
		}
		d.err = err
		d.mu.Unlock()
		return nil, err
	}
	d.answered("")
	return v, err
}

// answered notes that the server answered, at ctl if that is new, and calls
// back if it had not.
func (d *Server) answered(ctl string) {
	d.mu.Lock()
	wasDown, back := d.err != nil, d.back
	d.err, d.told = nil, ""
	if ctl != "" && ctl != d.ctl {
		d.ctl = ctl
		d.logf("source: DLNA server %q at %s", d.name, ctl)
	} else if wasDown {
		d.logf("source: the DLNA server %q answers again", d.name)
	}
	d.mu.Unlock()
	if wasDown && back != nil {
		back()
	}
}

// find looks for the server and reads its description.
func (d *Server) find(ctx context.Context) error {
	loc, ip, err := locate(ctx, d.addr)
	var dev renderer.Device
	if err == nil {
		dev, err = renderer.Describe(ctx, loc, ip)
	}
	ctl := dev.Control(strings.TrimSuffix(ContentDirectory, "1"))
	if err == nil && ctl == "" {
		err = fmt.Errorf("%s has no ContentDirectory", loc)
	}
	if err != nil {
		d.mu.Lock()
		d.err = err
		if err.Error() != d.told {
			d.told = err.Error()
			d.logf("source: the DLNA server is not found: %v; looking for it every minute", err)
		}
		d.mu.Unlock()
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, sourceWait)
	defer cancel()
	caps := ""
	if v, err := renderer.Call(cctx, ctl, ContentDirectory, "GetSearchCapabilities", nil); err == nil {
		caps = strings.TrimSpace(v["SearchCaps"])
	}
	d.mu.Lock()
	d.ip, d.name, d.icon, d.caps = ip, dev.Name, dev.Icon, caps
	d.mu.Unlock()
	d.answered(ctl)
	return nil
}

// locate finds a DLNA server's description: the URL given, or the LOCATION
// its address answers a search with, sent to it on port 1900 whatever port
// was given, and to the multicast group. ip is the address it must be on.
func locate(ctx context.Context, addr string) (loc, ip string, err error) {
	if IsURL(addr) {
		u, err := url.Parse(addr)
		if err != nil {
			return "", "", err
		}
		return addr, u.Hostname(), nil
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return "", "", fmt.Errorf("%s: no IPv4 address: %v", host, err)
	}
	ip = ips[0].String()
	// Some servers (minidlna) only hear the multicast group: it is asked too,
	// and only the address's answer is taken.
	locs, err := renderer.Search(ContentDirectory, 2*time.Second, &net.UDPAddr{IP: ips[0], Port: 1900}, renderer.Group)
	if err != nil {
		return "", "", err
	}
	if l := pick(locs, ip); l != "" {
		return l, ip, nil
	}
	return "", "", fmt.Errorf("no DLNA server answers at %s", ip)
}

// pick is the LOCATION that ip answered with and that is on ip: a server can
// also answer with another of its addresses (Emby on Windows gives one on
// 127.0.0.1 too), which the bridge cannot reach it on.
func pick(locs map[string]string, ip string) string {
	var on []string
	for l, from := range locs {
		if u, err := url.Parse(l); err == nil && from == ip && u.Hostname() == ip {
			on = append(on, l)
		}
	}
	if len(on) == 0 {
		return ""
	}
	slices.Sort(on) // the same one each time
	return on[0]
}
