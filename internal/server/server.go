// Package server is the UPnP media server: it describes itself, answers the
// TVs' browse calls with the DVR's channels and recordings, serves the streams
// that go through it, and has the settings page.
package server

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
	"dlna/internal/ssdp"
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
	// Settings say how each client is served.
	Settings *config.Store
	FFmpeg   string
	// SelfURL is this server's address as ffmpeg reaches it, for the streams
	// it reads back from here.
	SelfURL   string
	Bookmarks *Bookmarks
	// Debug logs each SOAP request and the start of its response.
	Debug bool
	Logf  func(format string, a ...any)
	// TrustedProxies are the proxies whose X-Forwarded-For and X-Forwarded-Host
	// are believed. Empty: no headers are, and the connection's address is the
	// client's.
	TrustedProxies []*net.IPNet
	// PlansDir is where the cut lists of recordings are kept between runs. Empty:
	// they are not.
	PlansDir string
	// ClientName is what the DVR calls this program in its client list and
	// activity list. Empty: it is not registered, and the DVR names it by address.
	ClientName string
	// Logs is what the page shows of the log; Logf should write to it.
	Logs *LogBuffer
	// Discovery reports how discovery has gone.
	Discovery func() ssdp.Stats
	// ConfigPath is where the settings are kept, for the page to say.
	ConfigPath string
}

type Server struct {
	started time.Time
	now     func() time.Time // time.Now, but a test can set it
	ffmpeg  ffmpegCheck
	cfg     Config
	plans   plans
	clients clientLog
	jobs    jobs
	notices chan struct{} // limits event notices going out at once

	regMu sync.Mutex
	reg   registration
	// regEvery is how often the registration is renewed, and regRetry the
	// shortest and longest wait after a failure; tests shorten them.
	regEvery                 time.Duration
	regRetryMin, regRetryMax time.Duration
}

func New(cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Settings == nil {
		cfg.Settings, _ = config.Open("", config.Config{})
	}
	if cfg.SelfURL == "" {
		cfg.SelfURL = "http://127.0.0.1:8200"
	}
	s := &Server{cfg: cfg, started: time.Now(), now: time.Now, notices: make(chan struct{}, 8)}
	s.plans.dir, s.plans.logf = cfg.PlansDir, cfg.Logf
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.ui)
	mux.HandleFunc("GET /api/state", s.apiState)
	mux.HandleFunc("GET /api/log", s.apiLog)
	mux.HandleFunc("PUT /api/config", s.apiSetConfig)
	mux.HandleFunc("GET /channels.m3u", s.playlist)
	mux.HandleFunc("GET /description.xml", s.description)
	mux.HandleFunc("GET /icons/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, typ, ok := iconFile(r.PathValue("name"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", typ)
		w.Write(b)
	})
	mux.HandleFunc("GET /scpd/ContentDirectory.xml", serveXML(contentDirectoryXML))
	mux.HandleFunc("GET /scpd/ConnectionManager.xml", serveXML(connectionManagerXML))
	mux.HandleFunc("POST /ctl/ContentDirectory", s.contentDirectory)
	mux.HandleFunc("POST /ctl/ConnectionManager", s.connectionManager)
	mux.HandleFunc("/evt/{service}", s.events)
	mux.HandleFunc("GET /stream/live/{number}", s.streamLive)
	mux.HandleFunc("GET /stream/rec/{id}", s.streamRecording)
	mux.HandleFunc("GET /stream/adfree/{id}", s.streamAdFree)
	return logRequests(s.cfg.Logf, s.cfg.Version, s.who, mux)
}

func logRequests(logf func(string, ...any), version string, who func(*http.Request) string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", serverHeader(version))
		// The settings page polls every few seconds and would drown the rest.
		if p := r.URL.Path; !strings.HasPrefix(p, "/ctl/") && !strings.HasPrefix(p, "/api/") && !strings.HasPrefix(p, "/icons/") && p != "/" {
			logf("http: %s %s %s from %s", r.Method, r.URL.Path, r.Header.Get("User-Agent"), who(r))
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

// --- ContentDirectory

func (s *Server) contentDirectory(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Debug {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		s.cfg.Logf("debug: %s request from %s (%s): %s", r.URL.Path, s.who(r), r.Header.Get("SOAPAction"), body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		w = &recorder{ResponseWriter: w, logf: s.cfg.Logf, to: s.who(r)}
	}
	a, err := readAction(r.Body)
	if err != nil {
		writeFault(w, errInvalidAction)
		return
	}
	s.cfg.Logf("soap: %s %s %s from %s", a.Name, a.Args["BrowseFlag"], a.Args["ObjectID"], s.who(r))
	switch a.Name {
	case "Browse":
		out, err := s.browse(r.Context(), s.viewOf(r), a.Args)
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
const featureList = `<?xml version="1.0" encoding="UTF-8"?><Features xmlns="urn:schemas-upnp-org:av:avs" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="urn:schemas-upnp-org:av:avs http://www.upnp.org/schemas/av/avs.xsd"><Feature name="samsung.com_BASICVIEW" version="1"><container id="` + idLive + `" type="object.item.videoItem"/><container id="` + idFavorites + `" type="object.item.videoItem"/><container id="` + idRecordings + `" type="object.item.videoItem"/></Feature></Features>`

// The tree never changes shape, so the update id only says "the lists may be
// new": it moves on once a minute.
func updateID() string { return strconv.FormatInt(time.Now().Unix()/60%1000000, 10) }

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

const sourceProtocols = "http-get:*:" + videoMIME + ":*,http-get:*:" + tsMIME + ":*"

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
		if cb := callback(r.Header.Get("Callback"), s.clientIP(r)); !renewal && cb != "" {
			select {
			case s.notices <- struct{}{}:
				go func() {
					defer func() { <-s.notices }()
					s.sendEvent(cb, sid, props)
				}()
			default: // enough are going out already
			}
		}
	case "UNSUBSCRIBE":
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// callback takes the first address out of a CALLBACK header, but only one that
// is the subscriber's own: a TV's address and any port, over http. Anything else
// would have this server send requests wherever a client names.
func callback(h, from string) string {
	remote := net.ParseIP(from)
	if remote == nil {
		return ""
	}
	for _, part := range strings.Split(h, ">") {
		raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "<"))
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || u.User != nil || u.Host == "" {
			continue
		}
		ip := net.ParseIP(u.Hostname()) // a name would have to be looked up
		if ip == nil || !ip.Equal(remote) {
			continue
		}
		if p := u.Port(); p != "" {
			if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
				continue
			}
		}
		return u.String()
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
	req.Header.Set("User-Agent", s.agent())
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("Nt", "upnp:event")
	req.Header.Set("Nts", "upnp:propchange")
	req.Header.Set("Sid", sid)
	req.Header.Set("Seq", "0")
	c := &http.Client{
		Timeout: 5 * time.Second,
		// A redirect would send the request on to wherever the subscriber likes.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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

// recorder logs the status and the start of a response.
type recorder struct {
	http.ResponseWriter
	logf   func(string, ...any)
	to     string
	status int
	logged bool
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.logged {
		r.logged = true
		status := r.status
		if status == 0 {
			status = http.StatusOK
		}
		head := b
		if len(head) > 1500 {
			head = head[:1500]
		}
		r.logf("debug: response to %s: %d, %d bytes, %v: %s", r.to, status, len(b), r.Header(), head)
	}
	return r.ResponseWriter.Write(b)
}
