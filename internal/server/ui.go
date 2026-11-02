package server

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"

	"dlna/internal/config"
)

//go:embed ui.html
var uiHTML []byte

func (s *Server) ui(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiHTML)
}

type state struct {
	Name     string           `json:"name"`
	Version  string           `json:"version"`
	DVR      string           `json:"dvr"`
	DVRError string           `json:"dvr_error,omitempty"`
	Channels int              `json:"channels"`
	Recs     int              `json:"recordings"`
	Config   config.Config    `json:"config"`
	Saved    bool             `json:"saved"` // the config is kept in a file
	Clients  []seenClient     `json:"clients"`
	Jobs     []job            `json:"jobs"`
	Defaults config.Transcode `json:"transcode_defaults"`
}

func (s *Server) apiState(w http.ResponseWriter, r *http.Request) {
	st := state{
		Name: s.cfg.Name, Version: s.cfg.Version, DVR: s.cfg.DVR.Base.String(),
		Config: s.cfg.Settings.Get(), Saved: s.cfg.Settings.Persistent(),
		Clients: s.clients.list(), Jobs: s.jobs.list(), Defaults: config.DefaultTranscode(),
	}
	if chs, err := s.cfg.DVR.Channels(r.Context()); err != nil {
		st.DVRError = err.Error()
	} else {
		st.Channels = len(chs)
	}
	if recs, err := s.cfg.DVR.Recordings(r.Context()); err == nil {
		st.Recs = len(recs)
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
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
