// Package server is the UPnP media server: it describes itself, answers the
// TVs' browse calls with the DVR's channels and recordings, and serves the
// tiny TV's converted streams.
package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"dlna/internal/dvr"
)

const (
	cdService = "urn:schemas-upnp-org:service:ContentDirectory:1"
	cmService = "urn:schemas-upnp-org:service:ConnectionManager:1"
)

type Config struct {
	Name, UUID, Version string
	DVR                 *dvr.Client
	// DVRPublic is the DVR's address as the TVs reach it. Empty: the DVR
	// address, with a loopback host swapped for the one the TV used to reach
	// this server (they run on the same machine).
	DVRPublic string
	// Proxy sends the big TVs' streams through this server, which adds the
	// DLNA headers some TVs want, instead of pointing them at the DVR.
	Proxy     bool
	FFmpeg    string
	TinyMax   int // conversions at once
	Bookmarks *Bookmarks
	Logf      func(format string, a ...any)
}

type Server struct {
	cfg  Config
	tiny chan struct{}
}

func New(cfg Config) *Server {
	if cfg.TinyMax <= 0 {
		cfg.TinyMax = 2
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Server{cfg: cfg, tiny: make(chan struct{}, cfg.TinyMax)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /description.xml", s.description)
	mux.HandleFunc("GET /scpd/ContentDirectory.xml", serveXML(contentDirectoryXML))
	mux.HandleFunc("GET /scpd/ConnectionManager.xml", serveXML(connectionManagerXML))
	mux.HandleFunc("POST /ctl/ContentDirectory", s.contentDirectory)
	mux.HandleFunc("POST /ctl/ConnectionManager", s.connectionManager)
	mux.HandleFunc("/evt/{service}", s.events)
	mux.HandleFunc("GET /stream/live/{number}", s.proxyLive)
	mux.HandleFunc("GET /stream/rec/{id}", s.proxyRecording)
	mux.HandleFunc("GET /tiny/channels.m3u", s.tinyPlaylist)
	mux.HandleFunc("GET /tiny/{file}", s.tinyStream)
	return logRequests(s.cfg.Logf, mux)
}

func logRequests(logf func(string, ...any), h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/ctl/") {
			logf("http: %s %s %s from %s", r.Method, r.URL.Path, r.Header.Get("User-Agent"), r.RemoteAddr)
		}
		h.ServeHTTP(w, r)
	})
}

func serveXML(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		io.WriteString(w, body)
	}
}

func (s *Server) description(w http.ResponseWriter, r *http.Request) {
	body := strings.NewReplacer(
		"{{.Name}}", escape(s.cfg.Name),
		"{{.Version}}", escape(s.cfg.Version),
		"{{.UUID}}", escape(s.cfg.UUID),
	).Replace(descriptionXML)
	serveXML(body)(w, r)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s: DLNA bridge %s for the Channels DVR at %s\n\n", s.cfg.Name, s.cfg.Version, s.cfg.DVR.Base)
	chs, err := s.cfg.DVR.Channels(r.Context())
	if err != nil {
		fmt.Fprintf(w, "DVR: %v\n", err)
		return
	}
	fmt.Fprintf(w, "%d live channels. Tiny TV playlist: http://%s/tiny/channels.m3u\n", len(chs), r.Host)
}

// --- ContentDirectory

func (s *Server) contentDirectory(w http.ResponseWriter, r *http.Request) {
	a, err := readAction(r.Body)
	if err != nil {
		writeFault(w, errInvalidAction)
		return
	}
	s.cfg.Logf("soap: %s %s %s from %s", a.Name, a.Args["BrowseFlag"], a.Args["ObjectID"], r.RemoteAddr)
	switch a.Name {
	case "Browse":
		out, err := s.browse(r.Context(), r.Host, a.Args)
		if err != nil {
			s.fault(w, a, err)
			return
		}
		writeResponse(w, cdService, a.Name, out)
	case "GetSearchCapabilities":
		writeResponse(w, cdService, a.Name, [][2]string{{"SearchCaps", ""}})
	case "GetSortCapabilities":
		writeResponse(w, cdService, a.Name, [][2]string{{"SortCaps", ""}})
	case "GetSystemUpdateID":
		writeResponse(w, cdService, a.Name, [][2]string{{"Id", updateID()}})
	case "X_GetFeatureList":
		writeResponse(w, cdService, a.Name, [][2]string{{"FeatureList", featureList}})
	case "X_SetBookmark":
		pos, _ := strconv.Atoi(a.Args["PosSecond"])
		if err := s.cfg.Bookmarks.Set(a.Args["ObjectID"], pos); err != nil {
			s.cfg.Logf("bookmark: %v", err)
		}
		writeResponse(w, cdService, a.Name, nil)
	default:
		writeFault(w, errInvalidAction)
	}
}

func (s *Server) fault(w http.ResponseWriter, a action, err error) {
	se, ok := err.(*soapError)
	if !ok {
		s.cfg.Logf("soap: %s: %v", a.Name, err)
		se = &soapError{501, "Action Failed"}
	}
	writeFault(w, se)
}

// Samsung TVs ask for this before they show the server's folders.
const featureList = `<?xml version="1.0" encoding="UTF-8"?><Features xmlns="urn:schemas-upnp-org:av:avs" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="urn:schemas-upnp-org:av:avs http://www.upnp.org/schemas/av/avs.xsd"><Feature name="samsung.com_BASICVIEW" version="1"><container id="` + idLive + `" type="object.item.videoItem"/><container id="` + idRecordings + `" type="object.item.videoItem"/></Feature></Features>`

// The tree never changes shape, so the update id only says "the lists may be
// new": it moves on once a minute.
func updateID() string { return strconv.FormatInt(time.Now().Unix()/60%1000000, 10) }

func (s *Server) browse(ctx context.Context, host string, args map[string]string) ([][2]string, error) {
	id := args["ObjectID"]
	start, err1 := atoiOr(args["StartingIndex"], 0)
	count, err2 := atoiOr(args["RequestedCount"], 0)
	if err1 != nil || err2 != nil || start < 0 || count < 0 {
		return nil, errInvalidArgs
	}
	var objs []object
	total := 1
	switch args["BrowseFlag"] {
	case "BrowseMetadata":
		o, err := s.lookup(ctx, host, id)
		if err != nil {
			return nil, err
		}
		objs = []object{o}
	case "BrowseDirectChildren":
		all, err := s.children(ctx, host, id)
		if err != nil {
			return nil, err
		}
		total = len(all)
		if start > len(all) {
			start = len(all)
		}
		all = all[start:]
		if count > 0 && count < len(all) {
			all = all[:count]
		}
		objs = all
	default:
		return nil, errInvalidArgs
	}
	return [][2]string{
		{"Result", didl(objs)},
		{"NumberReturned", strconv.Itoa(len(objs))},
		{"TotalMatches", strconv.Itoa(total)},
		{"UpdateID", updateID()},
	}, nil
}

func atoiOr(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}

// Object ids. Channels are live/<number>; recordings are rec/<file id>, in
// folders recgroup/<group id>.
const (
	idRoot       = "0"
	idLive       = "live"
	idRecordings = "rec"
)

func (s *Server) children(ctx context.Context, host, id string) ([]object, error) {
	switch {
	case id == idRoot:
		live, err := s.lookup(ctx, host, idLive)
		if err != nil {
			return nil, err
		}
		rec, err := s.lookup(ctx, host, idRecordings)
		if err != nil {
			return nil, err
		}
		return []object{live, rec}, nil
	case id == idLive:
		return s.liveItems(ctx, host)
	case id == idRecordings:
		folders, _, err := s.recordingTree(ctx, host)
		return folders, err
	case strings.HasPrefix(id, "recgroup/"):
		_, items, err := s.recordingTree(ctx, host)
		if err != nil {
			return nil, err
		}
		var out []object
		for _, o := range items {
			if o.Parent == id {
				out = append(out, o)
			}
		}
		if out == nil {
			return nil, errNoSuchObject
		}
		return out, nil
	case strings.HasPrefix(id, "live/"), strings.HasPrefix(id, "rec/"):
		return nil, nil // items have no children
	}
	return nil, errNoSuchObject
}

func (s *Server) lookup(ctx context.Context, host, id string) (object, error) {
	switch {
	case id == idRoot:
		return object{ID: idRoot, Parent: "-1", Title: s.cfg.Name, Folder: true, Children: 2}, nil
	case id == idLive:
		chs, err := s.cfg.DVR.Channels(ctx)
		if err != nil {
			return object{}, err
		}
		return object{ID: idLive, Parent: idRoot, Title: "Live TV", Folder: true, Children: len(chs)}, nil
	case id == idRecordings:
		folders, _, err := s.recordingTree(ctx, host)
		if err != nil {
			return object{}, err
		}
		return object{ID: idRecordings, Parent: idRoot, Title: "Recordings", Folder: true, Children: len(folders)}, nil
	case strings.HasPrefix(id, "live/"):
		items, err := s.liveItems(ctx, host)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	case strings.HasPrefix(id, "recgroup/"):
		folders, _, err := s.recordingTree(ctx, host)
		if err != nil {
			return object{}, err
		}
		return find(folders, id)
	case strings.HasPrefix(id, "rec/"):
		_, items, err := s.recordingTree(ctx, host)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	}
	return object{}, errNoSuchObject
}

func find(objs []object, id string) (object, error) {
	for _, o := range objs {
		if o.ID == id {
			return o, nil
		}
	}
	return object{}, errNoSuchObject
}

func (s *Server) liveItems(ctx context.Context, host string) ([]object, error) {
	chs, err := s.cfg.DVR.Channels(ctx)
	if err != nil {
		return nil, err
	}
	// Without the guide the channels still play, just without show titles.
	now, err := s.cfg.DVR.NowPlaying(ctx)
	if err != nil {
		s.cfg.Logf("guide: %v", err)
	}
	out := make([]object, 0, len(chs))
	for _, ch := range chs {
		title := ch.Number + " " + ch.Name
		if show := now[ch.Number]; show != "" {
			title += " - " + show
		}
		out = append(out, object{
			ID: "live/" + ch.Number, Parent: idLive, Title: title, Class: classLive,
			URL:      s.streamURL(host, dvr.LivePath(ch.Number), "/stream/live/"+url.PathEscape(ch.Number)),
			Protocol: liveProtocol, Art: ch.Logo, ChannelNr: ch.Number, ChannelName: ch.Name,
		})
	}
	return out, nil
}

// recordingTree is the show folders and the recordings in them.
func (s *Server) recordingTree(ctx context.Context, host string) (folders, items []object, err error) {
	files, err := s.cfg.DVR.Recordings(ctx)
	if err != nil {
		return nil, nil, err
	}
	groups, err := s.cfg.DVR.Groups(ctx)
	if err != nil {
		s.cfg.Logf("groups: %v", err)
	}
	names := map[string]string{}
	art := map[string]string{}
	for _, g := range groups {
		names[string(g.ID)], art[string(g.ID)] = g.Name, g.Image
	}
	count := map[string]int{}
	var order []string
	for _, f := range files {
		gid := string(f.GroupID)
		if gid == "" {
			gid = "_" + f.Airing.Title // no folder from the DVR: group by show title
		}
		if count[gid] == 0 {
			order = append(order, gid)
			if names[gid] == "" {
				names[gid] = f.Airing.Title
			}
			if art[gid] == "" {
				art[gid] = f.Airing.Image
			}
		}
		count[gid]++
		parent := "recgroup/" + gid
		id := "rec/" + string(f.ID)
		bm := s.cfg.Bookmarks.Get(id)
		if bm == 0 && !f.Completed {
			bm = int(f.PlaybackTime) // where the Channels apps left off
		}
		img := f.Thumbnail
		if img == "" {
			img = f.Airing.Image
		}
		items = append(items, object{
			ID: id, Parent: parent, Title: recordingTitle(f), Class: classRecorded,
			URL:      s.streamURL(host, dvr.RecordingPath(string(f.ID)), "/stream/rec/"+url.PathEscape(string(f.ID))),
			Protocol: recProtocol, Duration: f.Duration, Art: img, Description: f.Airing.Summary,
			Date: f.Created(), Bookmark: bm,
		})
	}
	for _, gid := range order {
		title := names[gid]
		if title == "" {
			title = "Other"
		}
		folders = append(folders, object{ID: "recgroup/" + gid, Parent: idRecordings, Title: title, Folder: true, Children: count[gid], Art: art[gid]})
	}
	sort.SliceStable(folders, func(i, j int) bool { return strings.ToLower(folders[i].Title) < strings.ToLower(folders[j].Title) })
	return folders, items, nil
}

// recordingTitle names an episode by season, episode and title, or by date
// when the guide gave neither.
func recordingTitle(f dvr.Recording) string {
	a := f.Airing
	var parts []string
	if a.Season > 0 && a.Episode > 0 {
		parts = append(parts, fmt.Sprintf("S%02dE%02d", a.Season, a.Episode))
	}
	if a.EpisodeTitle != "" {
		parts = append(parts, a.EpisodeTitle)
	}
	if len(parts) == 0 {
		parts = append(parts, a.Title)
		if f.CreatedAt > 0 {
			parts = append(parts, f.Created().Format("2006-01-02"))
		}
	}
	return strings.Join(parts, " ")
}

// streamURL is where a TV plays an item: the DVR itself, or this server in
// proxy mode.
func (s *Server) streamURL(host, dvrPath, proxyPath string) string {
	if s.cfg.Proxy {
		return "http://" + host + proxyPath
	}
	return s.dvrPublic(host) + dvrPath
}

func (s *Server) dvrPublic(host string) string {
	if s.cfg.DVRPublic != "" {
		return strings.TrimRight(s.cfg.DVRPublic, "/")
	}
	u := *s.cfg.DVR.Base
	if isLoopback(u.Hostname()) {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			h = host
		}
		port := u.Port()
		if port == "" {
			port = "80"
		}
		u.Host = net.JoinHostPort(strings.Trim(h, "[]"), port)
	}
	return u.String()
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// --- ConnectionManager

func (s *Server) connectionManager(w http.ResponseWriter, r *http.Request) {
	a, err := readAction(r.Body)
	if err != nil {
		writeFault(w, errInvalidAction)
		return
	}
	switch a.Name {
	case "GetProtocolInfo":
		writeResponse(w, cmService, a.Name, [][2]string{{"Source", sourceProtocols}, {"Sink", ""}})
	case "GetCurrentConnectionIDs":
		writeResponse(w, cmService, a.Name, [][2]string{{"ConnectionIDs", "0"}})
	case "GetCurrentConnectionInfo":
		writeResponse(w, cmService, a.Name, [][2]string{
			{"RcsID", "-1"}, {"AVTransportID", "-1"}, {"ProtocolInfo", ""},
			{"PeerConnectionManager", ""}, {"PeerConnectionID", "-1"},
			{"Direction", "Output"}, {"Status", "OK"},
		})
	default:
		writeFault(w, errInvalidAction)
	}
}

const sourceProtocols = "http-get:*:" + videoMIME + ":*,http-get:*:" + tinyMIME + ":*"

// --- Eventing. Nothing here changes in a way worth pushing, but some TVs will
// not use a server that refuses subscriptions, and wait for the first event.

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	var props [][2]string
	switch service {
	case "ContentDirectory":
		props = [][2]string{{"SystemUpdateID", updateID()}, {"ContainerUpdateIDs", ""}}
	case "ConnectionManager":
		props = [][2]string{{"SourceProtocolInfo", sourceProtocols}, {"SinkProtocolInfo", ""}, {"CurrentConnectionIDs", "0"}}
	default:
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case "SUBSCRIBE":
		sid := r.Header.Get("Sid")
		renewal := sid != ""
		if !renewal {
			sid = "uuid:" + newUUID()
		}
		w.Header().Set("Sid", sid)
		w.Header().Set("Timeout", "Second-1800")
		w.Header().Set("Server", serverHeader(s.cfg.Version))
		w.WriteHeader(http.StatusOK)
		if cb := callback(r.Header.Get("Callback")); !renewal && cb != "" {
			go s.sendEvent(cb, sid, props)
		}
	case "UNSUBSCRIBE":
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// callback takes the first http:// address out of a CALLBACK header.
func callback(h string) string {
	for _, part := range strings.Split(h, ">") {
		u := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "<"))
		if strings.HasPrefix(u, "http://") {
			return u
		}
	}
	return ""
}

func (s *Server) sendEvent(cb, sid string, props [][2]string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0">`)
	for _, p := range props {
		b.WriteString("<e:property><" + p[0] + ">" + escape(p[1]) + "</" + p[0] + "></e:property>")
	}
	b.WriteString("</e:propertyset>")
	req, err := http.NewRequest("NOTIFY", cb, strings.NewReader(b.String()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("Nt", "upnp:event")
	req.Header.Set("Nts", "upnp:propchange")
	req.Header.Set("Sid", sid)
	req.Header.Set("Seq", "0")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		s.cfg.Logf("event to %s: %v", cb, err)
		return
	}
	resp.Body.Close()
}

func serverHeader(version string) string {
	return "Linux UPnP/1.0 DLNADOC/1.50 dlna/" + version
}

// ServerHeader is the SERVER header for SSDP too.
func ServerHeader(version string) string { return serverHeader(version) }
