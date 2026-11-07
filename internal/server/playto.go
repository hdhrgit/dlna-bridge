package server

import (
	"context"
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

// apiBrowse lists a folder for the page's library: its folders and items, with
// their pictures, as a TV browsing it would see them.
func (s *Server) apiBrowse(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		id = idRoot
	}
	kids, err := s.children(r.Context(), s.viewOf(r), id)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	type entry struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Folder   bool    `json:"folder"`
		Art      string  `json:"art,omitempty"`
		Duration float64 `json:"duration,omitempty"`
	}
	// A channel shows the picture of the show on now, as the page is for
	// choosing what to watch; the TVs keep the channel logos.
	var now map[string]string
	out := []entry{}
	v := s.viewOf(r)
	for _, o := range kids {
		art := o.Art
		if o.Class == classLive {
			if now == nil {
				now, _ = s.cfg.DVR.NowImages(r.Context())
			}
			if img := now[o.ChannelNr]; img != "" {
				art = s.artURL(v, img)
			}
		}
		out = append(out, entry{o.ID, o.Title, o.Folder, art, o.Duration})
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
	// Its client: by address, or by any User-Agent it has used (a speaker
	// browses as one program and fetches as another).
	name, prof := s.cfg.Settings.Match(ip, "", "")
	if name == "" {
		for _, c := range s.clients.list() {
			if c.IP != rd.IP {
				continue
			}
			if n, p := s.cfg.Settings.Match(ip, c.UserAgent, ""); n != "" {
				name, prof = n, p
				break
			}
		}
	}
	v := view{host: host, name: name, prof: prof}
	if rd.AudioOnly {
		// A player for sound gets a channel's sound: its Listen item.
		switch {
		case strings.HasPrefix(id, "live/"), strings.HasPrefix(id, "fav/"):
			_, n, _ := strings.Cut(id, "/")
			id = idListen + "/" + n
		case strings.HasPrefix(id, "srcch/"):
			id = "lsrcch/" + strings.TrimPrefix(id, "srcch/")
		case strings.HasPrefix(id, idOthers+"/live/"):
			id = idOthers + "/listen/" + strings.TrimPrefix(id, idOthers+"/live/")
		case strings.HasPrefix(id, idOthers+"/listen/"):
		case strings.HasPrefix(id, "listen/"), strings.HasPrefix(id, "lsrcch/"):
		default:
			return errString(rd.Name + " plays sound only: pick a live channel")
		}
	}
	o, err := s.lookup(r.Context(), v, id)
	if err != nil || o.Folder || o.URL == "" {
		return errString("nothing to play there")
	}
	if parts := strings.Split(o.Protocol, ":"); len(parts) >= 3 && !rd.Takes(parts[2]) {
		return errString(rd.Name + " does not play " + parts[2] + "; set its client's format (for sound: AAC or MP3)")
	}
	s.cfg.Logf("play on %s (%s): %s", rd.Name, rd.IP, o.Title)
	if err := rd.Play(r.Context(), o.URL, didl([]object{o})); err != nil {
		return err
	}
	if at := joinAt(o); at > 0 {
		// Joining another client: go to where it is, once the TV has started.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for i := 0; i < 10; i++ {
				time.Sleep(time.Second)
				if st, err := rd.State(ctx); err == nil && st.State == "PLAYING" {
					break
				}
			}
			if err := rd.Seek(ctx, int(at.Seconds())); err != nil {
				s.cfg.Logf("play on %s: joining at %s: %v", rd.Name, at, err)
			}
		}()
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }
