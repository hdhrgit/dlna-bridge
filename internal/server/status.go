package server

import (
	"context"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ffmpegCheck runs ffmpeg once, to say whether conversions can work.
type ffmpegCheck struct {
	once sync.Once
	info string
	ok   bool
}

func (s *Server) ffmpegInfo() (string, bool) {
	s.ffmpeg.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, s.cfg.FFmpeg, "-version").Output()
		if err != nil {
			s.ffmpeg.info = err.Error()
			return
		}
		line, _, _ := strings.Cut(string(out), "\n")
		s.ffmpeg.info, s.ffmpeg.ok = line, true
	})
	return s.ffmpeg.info, s.ffmpeg.ok
}

func (s *Server) apiLog(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if s.cfg.Logs == nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []LogLine{}, "last": 0})
		return
	}
	lines, last := s.cfg.Logs.Since(after)
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "last": last})
}
