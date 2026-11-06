package server

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
	"dlna/internal/ssdp"
)

//go:embed ui.html
var uiHTML []byte

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // a new version shows at once
	w.Write(uiHTML)
}

type state struct {
	Name      string        `json:"name"`
	Version   string        `json:"version"`
	DVR       string        `json:"dvr"`
	DVRError  string        `json:"dvr_error,omitempty"`
	DVRMillis int64         `json:"dvr_ms"`
	DVRHealth []dvr.Health  `json:"dvr_health"`
	DVRClient *registration `json:"dvr_client,omitempty"` // how the bridge is registered with the DVR
	Activity  []string      `json:"dvr_activity"`
	// ActivityOwn says Activity is only what the DVR does for the bridge's
	// address; otherwise it is everything, the bridge's address being unknown.
	ActivityOwn bool             `json:"dvr_activity_own"`
	Channels    int              `json:"channels"`
	Recs        int              `json:"recordings"`
	Config      config.Config    `json:"config"`
	ConfigPath  string           `json:"config_path"`
	Saved       bool             `json:"saved"` // the config is kept in a file
	Clients     []seenClient     `json:"clients"`
	Streams     []jobView        `json:"streams"`
	Defaults    config.Transcode `json:"transcode_defaults"`
	UptimeSecs  int64            `json:"uptime_s"`
	FFmpeg      string           `json:"ffmpeg"`
	FFmpegOK    bool             `json:"ffmpeg_ok"`
	Discovery   *ssdp.Stats      `json:"discovery,omitempty"`
	Location    string           `json:"location"` // where a TV finds the description
	// Features a client can check for: "seek" is ?t=<seconds> on a converted
	// recording's stream.
	Features []string `json:"features"`
}

func (s *Server) apiState(w http.ResponseWriter, r *http.Request) {
	st := state{
		Name: s.cfg.Name, Version: s.cfg.Version, DVR: s.cfg.DVR.Base.String(),
		Config: s.cfg.Settings.Get(), ConfigPath: s.cfg.ConfigPath, Saved: s.cfg.Settings.Persistent(),
		Clients: s.clients.list(), Streams: s.jobs.list(), Defaults: config.DefaultTranscode(),
		DVRHealth: s.cfg.DVR.Health(), DVRClient: s.registration(), UptimeSecs: int64(time.Since(s.started).Seconds()), Location: "http://" + s.hostOf(r) + "/description.xml",
		Features: []string{"seek"},
	}
	st.FFmpeg, st.FFmpegOK = s.ffmpegInfo()
	if s.cfg.Discovery != nil {
		d := s.cfg.Discovery()
		st.Discovery = &d
	}
	if chs, err := s.cfg.DVR.Channels(r.Context()); err != nil {
		st.DVRError = err.Error()
	} else {
		st.Channels = len(chs)
	}
	if recs, err := s.cfg.DVR.Recordings(r.Context()); err == nil {
		st.Recs = len(recs)
	}
	if items, took, err := s.cfg.DVR.Activity(r.Context()); err == nil {
		st.DVRMillis = took.Milliseconds()
		ip := ""
		if st.DVRClient != nil {
			ip = st.DVRClient.IP
		}
		st.ActivityOwn = ip != ""
		for _, it := range items {
			if ip == "" || it.IP == ip {
				st.Activity = append(st.Activity, it.Text)
			}
		}
	}
	if st.Activity == nil {
		st.Activity = []string{}
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) apiSetConfig(w http.ResponseWriter, r *http.Request) {
	// A form on another site cannot send this content type without a preflight.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send JSON"})
		return
	}
	var c config.Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.cfg.Settings.Set(c); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.cfg.Logf("settings saved: %d clients", len(c.Clients))
	writeJSON(w, http.StatusOK, map[string]any{"config": s.cfg.Settings.Get()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Never from a cache: the page asks the same URL again until something
	// changes, and a browser that kept the first answer would never see it.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
