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
	avTransport       = "urn:schemas-upnp-org:service:AVTransport:1"
	renderingControl  = "urn:schemas-upnp-org:service:RenderingControl:1"
	connectionManager = "urn:schemas-upnp-org:service:ConnectionManager:1"
)

// Renderer is one device that plays what it is told to.
type Renderer struct {
	ID    string `json:"id"` // its UDN
	Name  string `json:"name"`
	Model string `json:"model"`
	IP    string `json:"ip"`
	// AudioOnly says it takes no video: a speaker, a receiver.
	AudioOnly bool `json:"audio_only"`
	// Types are the MIME types it says it plays; empty when it did not say.
	Types []string `json:"-"`
	avt   string   // control URLs
	rc    string
	cm    string
}

// A TV can take a while to answer while it switches what it plays.
var client = &http.Client{Timeout: 15 * time.Second,
	// A redirect would send the request wherever the device says.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// Discover asks the LAN for renderers and waits up to wait for answers.
func Discover(ctx context.Context, wait time.Duration) ([]Renderer, error) {
	locs, err := Search("urn:schemas-upnp-org:device:MediaRenderer:1", wait, Group)
	if err != nil {
		return nil, err
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

// Group is the SSDP multicast group.
var Group = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

// Search sends an SSDP M-SEARCH for st to each of to, the multicast group or
// one device, and waits up to wait for answers: their LOCATIONs, each with
// the address that answered.
func Search(st string, wait time.Duration, to ...*net.UDPAddr) (map[string]string, error) {
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	for range 2 { // UDP gets lost
		for _, a := range to {
			msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + a.String() + "\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n" +
				"ST: " + st + "\r\n\r\n"
			conn.WriteToUDP([]byte(msg), a)
		}
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
	return locs, nil
}

// Device is what a device's description says of it.
type Device struct {
	UDN, Name, Model string
	// Services are the control URLs of its services, by service type.
	Services map[string]string
	// Icon is the address of its largest icon, if it has one.
	Icon string
}

// Control is the control URL of its service of a type, of any version, as
// "urn:schemas-upnp-org:service:AVTransport:".
func (d Device) Control(prefix string) string {
	for t, u := range d.Services {
		if strings.HasPrefix(t, prefix) {
			return u
		}
	}
	return ""
}

// Describe reads a device description. It and the control URLs and icon in it
// must be on ip, the address that answered: anything else would have the
// bridge send requests wherever a device on the LAN likes.
func Describe(ctx context.Context, loc, ip string) (Device, error) {
	base, err := url.Parse(loc)
	if err != nil || base.Hostname() != ip {
		return Device{}, errors.New("description not on the device's address")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", loc, nil)
	resp, err := client.Do(req)
	if err != nil {
		return Device{}, err
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
			Icons []struct {
				Width int    `xml:"width"`
				URL   string `xml:"url"`
			} `xml:"iconList>icon"`
		} `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&d); err != nil {
		return Device{}, err
	}
	if d.URLBase != "" {
		if b, err := url.Parse(d.URLBase); err == nil {
			base = b
		}
	}
	on := func(raw string) string {
		u, err := base.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() != ip || (u.Scheme != "http" && u.Scheme != "https") {
			return ""
		}
		return u.String()
	}
	dev := Device{UDN: d.Device.UDN, Name: d.Device.Name, Model: d.Device.Model, Services: map[string]string{}}
	for _, s := range d.Device.Services {
		if u := on(s.Control); u != "" {
			dev.Services[strings.TrimSpace(s.Type)] = u
		}
	}
	width := 0
	for _, i := range d.Device.Icons {
		if u := on(i.URL); u != "" && i.Width >= width {
			dev.Icon, width = u, i.Width
		}
	}
	return dev, nil
}

// describe reads a renderer's description.
func describe(ctx context.Context, loc, ip string) (Renderer, error) {
	d, err := Describe(ctx, loc, ip)
	if err != nil {
		return Renderer{}, err
	}
	r := Renderer{ID: d.UDN, Name: d.Name, Model: d.Model, IP: ip,
		avt: d.Control("urn:schemas-upnp-org:service:AVTransport:"),
		rc:  d.Control("urn:schemas-upnp-org:service:RenderingControl:"),
		cm:  d.Control("urn:schemas-upnp-org:service:ConnectionManager:")}
	if r.ID == "" || r.avt == "" {
		return Renderer{}, errors.New("no AVTransport")
	}
	// What it plays: a sink list with no video in it is a player for sound.
	if v, err := call(ctx, r.cm, connectionManager, "GetProtocolInfo", nil); err == nil && v["Sink"] != "" {
		r.AudioOnly = !playsVideo(v["Sink"])
		r.Types = sinkTypes(v["Sink"])
	}
	return r, nil
}

// playsVideo says whether a sink protocol list takes a video stream. A
// playlist type (video/mpegurl, as a kitchen speaker lists) is not one.
func playsVideo(sink string) bool {
	for _, p := range strings.Split(sink, ",") {
		parts := strings.Split(p, ":")
		if len(parts) < 3 {
			continue
		}
		mime := strings.ToLower(strings.TrimSpace(parts[2]))
		if strings.HasPrefix(mime, "video/") && !strings.Contains(mime, "mpegurl") || mime == "*" {
			return true
		}
	}
	return false
}

// sinkTypes are the MIME types in a sink protocol list, lower case.
func sinkTypes(sink string) []string {
	var out []string
	for _, p := range strings.Split(sink, ",") {
		if parts := strings.Split(p, ":"); len(parts) >= 3 {
			out = append(out, strings.ToLower(strings.TrimSpace(parts[2])))
		}
	}
	return out
}

// Takes says whether it plays a MIME type, as far as it said: true when it
// did not say, or listed "*".
func (r Renderer) Takes(mime string) bool {
	if len(r.Types) == 0 {
		return true
	}
	mime = strings.ToLower(mime)
	for _, t := range r.Types {
		if t == "*" || t == mime {
			return true
		}
	}
	return false
}

// Fault is an action the device refused, with the UPnP error code it gave
// (0 for none).
type Fault struct {
	Code int
	Desc string
	msg  string
}

func (f *Fault) Error() string { return f.msg }

// maxAnswer is the longest answer read: a media server's list of 2000 items.
const maxAnswer = 16 << 20

// Call sends one SOAP action to a control URL and returns the values of the
// answer.
func Call(ctx context.Context, ctl, service, action string, args [][2]string) (map[string]string, error) {
	return call(ctx, ctl, service, action, args)
}

// escaper writes the five predefined entities and nothing else: Go's own
// writes numeric references (&#34;), which some devices do not read (minidlna
// takes them as they are in a search).
var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// call sends one SOAP action and returns the values of the answer.
func call(ctx context.Context, ctl, service, action string, args [][2]string) (map[string]string, error) {
	if ctl == "" {
		return nil, fmt.Errorf("%s: not offered", action)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<u:` + action + ` xmlns:u="` + service + `">`)
	for _, a := range args {
		b.WriteString("<" + a[0] + ">" + escaper.Replace(a[1]) + "</" + a[0] + ">")
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode != http.StatusOK {
		v := values(body)
		code, _ := strconv.Atoi(strings.TrimSpace(v["errorCode"]))
		return nil, &Fault{code, v["errorDescription"], fmt.Sprintf("%s: %s %s", action, resp.Status, faultText(body))}
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
	Title    string `json:"title"` // of what it plays, from its own metadata
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
		st.Title = metaTitle(pi["TrackMetaData"])
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

// metaTitle is the dc:title in a DIDL-Lite text, as a renderer reports what it
// plays, whoever started it.
func metaTitle(didl string) string {
	d := xml.NewDecoder(strings.NewReader(didl))
	in := false
	for {
		tok, err := d.Token()
		if err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			in = t.Name.Local == "title"
		case xml.CharData:
			if in {
				return strings.TrimSpace(string(t))
			}
		case xml.EndElement:
			in = false
		}
	}
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
