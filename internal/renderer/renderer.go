// Package renderer finds the TVs and players on the LAN that can be told what
// to play (UPnP MediaRenderers) and tells them: play this, pause, seek, volume.
package renderer

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	avTransport      = "urn:schemas-upnp-org:service:AVTransport:1"
	renderingControl = "urn:schemas-upnp-org:service:RenderingControl:1"
)

// Renderer is one device that plays what it is told to.
type Renderer struct {
	ID    string `json:"id"` // its UDN
	Name  string `json:"name"`
	Model string `json:"model"`
	IP    string `json:"ip"`
	avt   string // control URLs
	rc    string
}

// A TV can take a while to answer while it switches what it plays.
var client = &http.Client{Timeout: 15 * time.Second,
	// A redirect would send the request wherever the device says.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Discover asks the LAN for renderers and waits up to wait for answers.
func Discover(ctx context.Context, wait time.Duration) ([]Renderer, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"
	for range 2 { // UDP gets lost
		conn.WriteToUDP([]byte(msg), group)
	}
	conn.SetReadDeadline(time.Now().Add(wait))
	locs := map[string]string{} // location: the address that answered
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		for _, l := range strings.Split(string(buf[:n]), "\r\n") {
			if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "location") {
				locs[strings.TrimSpace(v)] = from.IP.String()
			}
		}
	}
	var out []Renderer
	seen := map[string]bool{}
	for loc, ip := range locs {
		r, err := describe(ctx, loc, ip)
		if err != nil || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	return out, nil
}

// describe reads a device description. Its control URLs must be on the
// address that answered: anything else would have the bridge send requests
// wherever a device on the LAN likes.
func describe(ctx context.Context, loc, ip string) (Renderer, error) {
	base, err := url.Parse(loc)
	if err != nil || base.Hostname() != ip {
		return Renderer{}, errors.New("description not on the device's address")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", loc, nil)
	resp, err := client.Do(req)
	if err != nil {
		return Renderer{}, err
	}
	defer resp.Body.Close()
	var d struct {
		URLBase string `xml:"URLBase"`
		Device  struct {
			UDN      string `xml:"UDN"`
			Name     string `xml:"friendlyName"`
			Model    string `xml:"modelName"`
			Services []struct {
				Type    string `xml:"serviceType"`
				Control string `xml:"controlURL"`
			} `xml:"serviceList>service"`
		} `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&d); err != nil {
		return Renderer{}, err
	}
	if d.URLBase != "" {
		if b, err := url.Parse(d.URLBase); err == nil {
			base = b
		}
	}
	r := Renderer{ID: d.Device.UDN, Name: d.Device.Name, Model: d.Device.Model, IP: ip}
	for _, s := range d.Device.Services {
		u, err := base.Parse(strings.TrimSpace(s.Control))
		if err != nil || u.Hostname() != ip || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		switch {
		case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:AVTransport:"):
			r.avt = u.String()
		case strings.HasPrefix(s.Type, "urn:schemas-upnp-org:service:RenderingControl:"):
			r.rc = u.String()
		}
	}
	if r.ID == "" || r.avt == "" {
		return Renderer{}, errors.New("no AVTransport")
	}
	return r, nil
}

// call sends one SOAP action and returns the values of the answer.
func call(ctx context.Context, ctl, service, action string, args [][2]string) (map[string]string, error) {
	if ctl == "" {
		return nil, fmt.Errorf("%s: not offered", action)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<u:` + action + ` xmlns:u="` + service + `">`)
	for _, a := range args {
		b.WriteString("<" + a[0] + ">")
		xml.EscapeText(&b, []byte(a[1]))
		b.WriteString("</" + a[0] + ">")
	}
	b.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)
	req, err := http.NewRequestWithContext(ctx, "POST", ctl, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+service+"#"+action+`"`)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s %s", action, resp.Status, faultText(body))
	}
	return values(body), nil
}

// values are the leaf elements of a SOAP answer, by local name.
func values(body []byte) map[string]string {
	out := map[string]string{}
	d := xml.NewDecoder(bytes.NewReader(body))
	var name string
	for {
		tok, err := d.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name = t.Name.Local
		case xml.CharData:
			if name != "" {
				out[name] += string(t)
			}
		case xml.EndElement:
			name = ""
		}
	}
}

func faultText(body []byte) string {
	v := values(body)
	if d := v["errorDescription"]; d != "" {
		return v["errorCode"] + " " + d
	}
	return v["errorCode"]
}

var instance = [2]string{"InstanceID", "0"}

// Play has the renderer play uri, described by the DIDL-Lite meta.
func (r Renderer) Play(ctx context.Context, uri, meta string) error {
	r.Stop(ctx) // some will not take a new URI while playing
	if _, err := call(ctx, r.avt, avTransport, "SetAVTransportURI", [][2]string{instance, {"CurrentURI", uri}, {"CurrentURIMetaData", meta}}); err != nil {
		return err
	}
	return r.Resume(ctx)
}

func (r Renderer) Resume(ctx context.Context) error {
	_, err := call(ctx, r.avt, avTransport, "Play", [][2]string{instance, {"Speed", "1"}})
	return err
}

func (r Renderer) Pause(ctx context.Context) error {
	_, err := call(ctx, r.avt, avTransport, "Pause", [][2]string{instance})
	return err
}

func (r Renderer) Stop(ctx context.Context) error {
	_, err := call(ctx, r.avt, avTransport, "Stop", [][2]string{instance})
	return err
}

// Seek goes to sec seconds into what is playing.
func (r Renderer) Seek(ctx context.Context, sec int) error {
	sec = max(sec, 0)
	t := fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	_, err := call(ctx, r.avt, avTransport, "Seek", [][2]string{instance, {"Unit", "REL_TIME"}, {"Target", t}})
	return err
}

// State is what the renderer is doing.
type State struct {
	State    string `json:"state"` // PLAYING, PAUSED_PLAYBACK, STOPPED, TRANSITIONING...
	Position int    `json:"position"`
	Duration int    `json:"duration"`
	Volume   int    `json:"volume"` // -1 when unknown
	Muted    bool   `json:"muted"`
	URI      string `json:"uri"`
}

func (r Renderer) State(ctx context.Context) (State, error) {
	st := State{Volume: -1}
	ti, err := call(ctx, r.avt, avTransport, "GetTransportInfo", [][2]string{instance})
	if err != nil {
		return st, err
	}
	st.State = ti["CurrentTransportState"]
	if pi, err := call(ctx, r.avt, avTransport, "GetPositionInfo", [][2]string{instance}); err == nil {
		st.Position, st.Duration, st.URI = clock(pi["RelTime"]), clock(pi["TrackDuration"]), pi["TrackURI"]
	}
	if v, err := call(ctx, r.rc, renderingControl, "GetVolume", [][2]string{instance, {"Channel", "Master"}}); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(v["CurrentVolume"])); err == nil {
			st.Volume = n
		}
	}
	if v, err := call(ctx, r.rc, renderingControl, "GetMute", [][2]string{instance, {"Channel", "Master"}}); err == nil {
		st.Muted = strings.TrimSpace(v["CurrentMute"]) == "1"
	}
	return st, nil
}

// SetVolume sets the volume, 0 to 100.
func (r Renderer) SetVolume(ctx context.Context, level int) error {
	level = min(max(level, 0), 100)
	_, err := call(ctx, r.rc, renderingControl, "SetVolume", [][2]string{instance, {"Channel", "Master"}, {"DesiredVolume", strconv.Itoa(level)}})
	return err
}

func (r Renderer) SetMute(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	_, err := call(ctx, r.rc, renderingControl, "SetMute", [][2]string{instance, {"Channel", "Master"}, {"DesiredMute", v}})
	return err
}

// clock reads H:MM:SS(.fff) as seconds; 0 for anything else.
func clock(s string) int {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0
	}
	sec := 0
	for _, p := range parts {
		p, _, _ = strings.Cut(p, ".")
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		sec = sec*60 + n
	}
	return sec
}

// Cache keeps the renderers found lately.
type Cache struct {
	mu   sync.Mutex
	list []Renderer
	at   time.Time
}

// List is the renderers, looked for again when older than maxAge.
func (c *Cache) List(ctx context.Context, maxAge time.Duration) []Renderer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list == nil || time.Since(c.at) > maxAge {
		if l, err := Discover(ctx, 3*time.Second); err == nil {
			c.list, c.at = l, time.Now()
		}
	}
	return append([]Renderer(nil), c.list...)
}

// Get is one renderer by its id, from the last look.
func (c *Cache) Get(ctx context.Context, id string) (Renderer, bool) {
	for _, r := range c.List(ctx, 10*time.Minute) {
		if r.ID == id {
			return r, true
		}
	}
	return Renderer{}, false
}
