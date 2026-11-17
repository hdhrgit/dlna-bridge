package server

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dlna/internal/config"
	"dlna/internal/renderer"
)

// A DLNA media server served in place of the DVR: the TVs' Browse and Search
// go on to it, with its own object ids, and what it answers comes back as the
// client's profile says. A client that sees only video or only sound is shown
// those items, by their class. Its streams are played from it directly,
// through the bridge (/stream/src/<object id>), or converted. While it does
// not answer, the root is empty.

// maxSourceItems is the most of a folder fetched to leave out what a client
// does not see.
const maxSourceItems = 2000

// filters says whether the client sees only video or only sound.
func (v view) filters() bool {
	return v.prof.Kind == config.KindVideo || v.prof.Kind == config.KindSound
}

// hidesClass says whether the client does not see an item of a DLNA source:
// one set to video sees video and pictures, one set to sound sees sound.
func (v view) hidesClass(o object) bool {
	if o.Folder {
		return false
	}
	audio := strings.HasPrefix(o.Class, "object.item.audioItem")
	switch v.prof.Kind {
	case config.KindVideo:
		return audio
	case config.KindSound:
		return !audio
	}
	return false
}

// sourceList asks the source for a list, a folder's children or a search, and
// makes its entries the client's. For a client that sees only some classes
// the whole list is fetched and paged here, so that the count stays right;
// otherwise start and count go to the source.
func (s *Server) sourceList(ctx context.Context, v view, action string, args [][2]string, start, count int) ([]object, int, error) {
	from, n := start, count
	if v.filters() {
		from, n = 0, maxSourceItems
	}
	args = append(args, [2]string{"Filter", "*"}, [2]string{"StartingIndex", strconv.Itoa(from)},
		[2]string{"RequestedCount", strconv.Itoa(n)}, [2]string{"SortCriteria", ""})
	out, err := s.cfg.DLNA.Call(ctx, action, args)
	if err != nil {
		return nil, 0, sourceError(err)
	}
	entries, err := parseDIDL(out["Result"])
	if err != nil {
		return nil, 0, err
	}
	total, _ := strconv.Atoi(out["TotalMatches"])
	var objs []object
	for _, e := range entries {
		if o, ok := s.sourceObject(v, e); ok && !v.hidesClass(o) {
			objs = append(objs, o)
		}
	}
	if v.filters() {
		total = len(objs)
		objs = objs[min(start, len(objs)):]
		if count > 0 && count < len(objs) {
			objs = objs[:count]
		}
	}
	return objs, total, nil
}

// sourceChildren is a folder of the source, or for its root while it does not
// answer, nothing.
func (s *Server) sourceChildren(ctx context.Context, v view, id string, start, count int) ([]object, int, error) {
	objs, total, err := s.sourceList(ctx, v, "Browse", [][2]string{{"ObjectID", id}, {"BrowseFlag", "BrowseDirectChildren"}}, start, count)
	if err != nil && id == idRoot && !isFault(err) {
		return nil, 0, nil
	}
	if id == idRoot {
		// The source may name another parent for them (Emby does).
		for i := range objs {
			objs[i].Parent = idRoot
		}
	}
	return objs, total, err
}

// sourceLookup is one object of the source. Its root has the bridge's name.
func (s *Server) sourceLookup(ctx context.Context, v view, id string) (object, error) {
	objs, _, err := s.sourceList(ctx, v, "Browse", [][2]string{{"ObjectID", id}, {"BrowseFlag", "BrowseMetadata"}}, 0, 0)
	if id == idRoot {
		if err != nil && !isFault(err) {
			objs, err = nil, nil
		}
		if err == nil {
			o := object{ID: idRoot, Parent: "-1", Folder: true}
			if len(objs) > 0 {
				o = objs[0]
			}
			o.Title = s.name()
			return o, nil
		}
	}
	if err != nil {
		return object{}, err
	}
	if len(objs) == 0 {
		return object{}, errNoSuchObject
	}
	return objs[0], nil
}

// sourceSearch passes a Search on, when the source searches.
func (s *Server) sourceSearch(ctx context.Context, v view, args map[string]string) ([][2]string, error) {
	if s.cfg.DLNA.SearchCaps() == "" {
		return nil, errInvalidAction
	}
	start, err1 := atoiOr(args["StartingIndex"], 0)
	count, err2 := atoiOr(args["RequestedCount"], 0)
	if err1 != nil || err2 != nil || start < 0 || count < 0 {
		return nil, errInvalidArgs
	}
	objs, total, err := s.sourceList(ctx, v, "Search", [][2]string{{"ContainerID", args["ContainerID"]}, {"SearchCriteria", args["SearchCriteria"]}}, start, count)
	if err != nil {
		return nil, err
	}
	return [][2]string{
		{"Result", didl(objs)},
		{"NumberReturned", strconv.Itoa(len(objs))},
		{"TotalMatches", strconv.Itoa(total)},
		{"UpdateID", s.systemUpdateID()},
	}, nil
}

// sourceError passes a fault of the source's on to the client as its own.
func sourceError(err error) error {
	var f *renderer.Fault
	if errors.As(err, &f) && f.Code > 0 {
		return &soapError{f.Code, f.Desc}
	}
	return err
}

func isFault(err error) bool {
	_, ok := err.(*soapError)
	return ok
}

// didlEntry is a container or item as a DIDL-Lite list gives it.
type didlEntry struct {
	XMLName     xml.Name
	ID          string   `xml:"id,attr"`
	Parent      string   `xml:"parentID,attr"`
	ChildCount  string   `xml:"childCount,attr"`
	Title       string   `xml:"title"`
	Class       string   `xml:"class"`
	Date        string   `xml:"date"`
	Description string   `xml:"description"`
	Art         []string `xml:"albumArtURI"`
	Res         []struct {
		Protocol string     `xml:"protocolInfo,attr"`
		Duration string     `xml:"duration,attr"`
		Attrs    []xml.Attr `xml:",any,attr"`
		URL      string     `xml:",chardata"`
	} `xml:"res"`
}

// resAttrs are the res attributes passed on with a stream that is not
// converted: TVs show and seek by them.
var resAttrs = []string{"size", "resolution", "bitrate", "nrAudioChannels", "sampleFrequency"}

func parseDIDL(s string) ([]didlEntry, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var d struct {
		Entries []didlEntry `xml:",any"`
	}
	err := xml.Unmarshal([]byte(s), &d)
	return d.Entries, err
}

// onSource says whether an address is on the source: the only one whose
// streams and pictures the bridge fetches.
func (s *Server) onSource(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() == s.cfg.DLNA.IP()
}

// sourceObject makes an entry the client's: its picture through the bridge,
// its stream as the profile says. An item with no http stream on the source
// is left out.
func (s *Server) sourceObject(v view, e didlEntry) (object, bool) {
	o := object{ID: e.ID, Parent: e.Parent, Title: e.Title, Description: e.Description}
	for _, a := range e.Art {
		if s.onSource(a) {
			o.Art = s.artURL(v, strings.TrimSpace(a))
			break
		}
	}
	if e.XMLName.Local == "container" {
		o.Folder = true
		o.Children, _ = strconv.Atoi(e.ChildCount)
		return o, true
	}
	if e.XMLName.Local != "item" {
		return o, false
	}
	o.Class = strings.TrimSpace(e.Class)
	o.Date = parseDate(e.Date)
	if o.Art == "" {
		o.Art = "http://" + v.host + "/icons/logo120.png" // as for a recording without one
	}
	for _, r := range e.Res {
		u := strings.TrimSpace(r.URL)
		if !strings.HasPrefix(r.Protocol, "http-get:") || !s.onSource(u) {
			continue
		}
		o.Duration = parseClock(r.Duration)
		bridge := "/stream/src/" + url.PathEscape(e.ID)
		converted := false
		switch {
		case strings.HasPrefix(o.Class, "object.item.videoItem"):
			res := s.resource(v, u, bridge, nil, false, r.Protocol)
			o.URL, o.Protocol = res.url, res.protocol
			converted = v.prof.Mode == config.ModeTranscode
		case v.prof.Mode == config.ModeDirect:
			o.URL, o.Protocol = u, r.Protocol
		case strings.HasPrefix(o.Class, "object.item.audioItem") && v.prof.Mode == config.ModeTranscode && v.prof.Audio.Format != config.AudioCopy:
			mime, features := audioFeatures(v.prof.Audio.Format)
			o.URL, o.Protocol = s.bridgeURL(v, bridge, nil), "http-get:*:"+mime+":"+features
			converted = true
		default:
			o.URL, o.Protocol = s.bridgeURL(v, bridge, nil), r.Protocol
		}
		if !converted {
			for _, name := range resAttrs {
				for _, a := range r.Attrs {
					if a.Name.Local == name && a.Name.Space == "" && strings.TrimSpace(a.Value) != "" {
						o.ResAttrs = append(o.ResAttrs, [2]string{name, strings.TrimSpace(a.Value)})
					}
				}
			}
		}
		return o, true
	}
	return o, false
}

// parseDate reads a dc:date: a day, or a time with or without its zone.
func parseDate(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseClock reads a res duration, H:MM:SS.mmm, in seconds.
func parseClock(s string) float64 {
	sec := 0.0
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0
	}
	for _, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil || n < 0 {
			return 0
		}
		sec = sec*60 + n
	}
	return sec
}

// srcItem is what a stream of the source needs to know of its item.
type srcItem struct {
	url, mime, features, class, title string
	duration                          float64
	at                                time.Time
}

// srcItems keeps the items asked for lately, for srcItemAge: a TV opens a
// stream many times over.
type srcItems struct {
	mu sync.Mutex
	m  map[string]srcItem
}

const (
	srcItemAge  = 10 * time.Minute
	maxSrcItems = 5000
)

// sourceItem finds an item of the source, to stream it.
func (s *Server) sourceItem(ctx context.Context, id string) (srcItem, error) {
	c := &s.srcItems
	c.mu.Lock()
	it, ok := c.m[id]
	c.mu.Unlock()
	if ok && time.Since(it.at) < srcItemAge {
		return it, nil
	}
	out, err := s.cfg.DLNA.Call(ctx, "Browse", [][2]string{{"ObjectID", id}, {"BrowseFlag", "BrowseMetadata"},
		{"Filter", "*"}, {"StartingIndex", "0"}, {"RequestedCount", "0"}, {"SortCriteria", ""}})
	if err != nil {
		return srcItem{}, sourceError(err)
	}
	entries, err := parseDIDL(out["Result"])
	if err != nil {
		return srcItem{}, err
	}
	for _, e := range entries {
		for _, r := range e.Res {
			u := strings.TrimSpace(r.URL)
			parts := strings.SplitN(r.Protocol, ":", 4)
			if e.XMLName.Local != "item" || len(parts) < 4 || parts[0] != "http-get" || !s.onSource(u) {
				continue
			}
			it = srcItem{url: u, mime: parts[2], features: parts[3], class: strings.TrimSpace(e.Class), title: e.Title, duration: parseClock(r.Duration), at: time.Now()}
			if it.features == "*" || it.features == "" {
				it.features = recFeatures
			}
			c.mu.Lock()
			if c.m == nil || len(c.m) >= maxSrcItems {
				c.m = map[string]srcItem{}
			}
			c.m[id] = it
			c.mu.Unlock()
			return it, nil
		}
	}
	return srcItem{}, errNoSuchObject
}

// streamSource plays an item of the source to a client: passed through, or
// converted, video as the profile's Transcode and sound as its Audio says.
func (s *Server) streamSource(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.cfg.DLNA.Status(); err != nil {
		// Down: an item known from before would wait on it, as a TV gives up.
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	id := r.PathValue("id")
	it, err := s.sourceItem(r.Context(), id)
	if isFault(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	name, prof := s.profileFor(r)
	if prof.Mode == config.ModeTranscode {
		switch {
		case strings.HasPrefix(it.class, "object.item.videoItem"):
			s.transcode(w, r, prof.Transcode, name, it.title, it.url, nil, false, it.duration)
			return
		case strings.HasPrefix(it.class, "object.item.audioItem") && prof.Audio.Format != config.AudioCopy:
			s.convertAudio(w, r, name, prof, it.title, it.url, nil)
			return
		}
	}
	tw, done := s.tracked(w, r, "file", name, it.title, "")
	defer done()
	s.proxy(tw, r, it.url, it.mime, it.features)
}

// WatchSource looks for the DLNA server while it does not answer, and when it
// answers again tells the subscribed TVs to browse it anew.
func (s *Server) WatchSource(ctx context.Context) {
	s.cfg.DLNA.Run(ctx, func() { s.changedFolders("source", []string{idRoot}) })
}
