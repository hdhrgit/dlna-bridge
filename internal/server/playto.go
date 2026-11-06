package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"dlna/internal/renderer"
)

// Play on a TV: the settings page lists the TVs and players that can be told
// what to play, and tells one to play a channel or recording from the bridge,
// pause, seek or change the volume.

func (s *Server) apiRenderers(w http.ResponseWriter, r *http.Request) {
	maxAge := 5 * time.Minute
	if r.URL.Query().Get("refresh") != "" {
		maxAge = 0
	}
	list := s.renderers.List(r.Context(), maxAge)
	if list == nil {
		list = []renderer.Renderer{}
	}
	writeJSON(w, http.StatusOK, list)
}

// apiItems lists what can be played from a folder: Live TV, Recent
// recordings, Movies.
func (s *Server) apiItems(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	switch folder {
	case idLive, idFavorites, idRecent, idMovies, idRecNow:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no such folder"})
		return
	}
	kids, err := s.children(r.Context(), s.viewOf(r), folder)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	type item struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	out := []item{}
	for _, o := range kids {
		if !o.Folder {
			out = append(out, item{o.ID, o.Title})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// apiRenderer acts on one renderer: GET for its state, POST to play, pause,
// resume, stop, seek, set the volume or mute.
func (s *Server) apiRenderer(w http.ResponseWriter, r *http.Request) {
	rd, ok := s.renderers.Get(r.Context(), r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such TV; look again"})
		return
	}
	if r.Method == http.MethodGet {
		st, err := rd.State(r.Context())
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, st)
		return
	}
	// A form on another site cannot send this content type without a preflight.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send JSON"})
		return
	}
	var req struct {
		Object  string `json:"object"`
		Seconds int    `json:"seconds"`
		Level   int    `json:"level"`
		Mute    bool   `json:"mute"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx := r.Context()
	var err error
	switch r.PathValue("do") {
	case "play":
		err = s.playOn(r, rd, req.Object)
	case "pause":
		err = rd.Pause(ctx)
	case "resume":
		err = rd.Resume(ctx)
	case "stop":
		err = rd.Stop(ctx)
	case "seek":
		err = rd.Seek(ctx, req.Seconds)
	case "volume":
		err = rd.SetVolume(ctx, req.Level)
	case "mute":
		err = rd.SetMute(ctx, req.Mute)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such action"})
		return
	}
	if err != nil {
		s.cfg.Logf("play on %s: %s: %v", rd.Name, r.PathValue("do"), err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{})
}

// playOn tells a renderer to play an object, as it would get it when browsing:
// with its client profile, at the bridge's address as the page reached it.
func (s *Server) playOn(r *http.Request, rd renderer.Renderer, id string) error {
	host := s.hostOf(r)
	if h, _, err := net.SplitHostPort(host); err == nil && isLoopback(h) || isLoopback(host) {
		return errString("open this page by the bridge's LAN address, so the TV can reach the address it is given")
	}
	ip := net.ParseIP(rd.IP)
	ua := ""
	for _, c := range s.clients.list() {
		if c.IP == rd.IP {
			ua = c.UserAgent
		}
	}
	name, prof := s.cfg.Settings.Match(ip, ua, "")
	v := view{host: host, name: name, prof: prof}
	o, err := s.lookup(r.Context(), v, id)
	if err != nil || o.Folder || o.URL == "" {
		return errString("nothing to play there")
	}
	s.cfg.Logf("play on %s (%s): %s", rd.Name, rd.IP, o.Title)
	return rd.Play(r.Context(), o.URL, didl([]object{o}))
}

type errString string

func (e errString) Error() string { return string(e) }
