package server

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The tiny TV (ESP32-S3, 240x240) decodes only baseline H.264 (no B-frames, no
// CABAC) and mono audio. The DVR's transcoder makes Main profile with
// B-frames, so each viewer gets an ffmpeg that turns the DVR's small stream
// into one the board can play.
const (
	tinySourceHeight = 180 // asked of the DVR; must be even
	tinySourceKbps   = 600 // total, with 64 for audio
	tinyFPS          = "24"
)

func tinyArgs(source string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", source,
		"-map", "0:v:0", "-map", "0:a:0?",
		// Fit 240x240, then round each side down to even: 16:9 comes out
		// 240x134, 4:3 240x180.
		"-vf", "fps=" + tinyFPS + ",scale=240:240:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p",
		"-c:v", "libx264", "-profile:v", "baseline", "-level:v", "3.0",
		"-preset", "veryfast", "-tune", "zerolatency",
		"-g", "48", "-b:v", "250k", "-maxrate", "300k", "-bufsize", "600k",
		"-a53cc", "0", // no caption data for the board to skip
		"-c:a", "aac", "-ac", "1", "-ar", "44100", "-b:a", "64k",
		"-f", "mpegts", "-muxdelay", "0", "pipe:1",
	}
}

// tinyStream serves /tiny/<number>.ts: a live MPEG-TS for the board. It has no
// length and does not seek.
func (s *Server) tinyStream(w http.ResponseWriter, r *http.Request) {
	number, ok := strings.CutSuffix(r.PathValue("file"), ".ts")
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, found, err := s.cfg.DVR.Channel(r.Context(), number)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", tinyMIME)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	if r.Method == http.MethodHead {
		return
	}
	select {
	case s.tiny <- struct{}{}:
		defer func() { <-s.tiny }()
	default:
		http.Error(w, fmt.Sprintf("already converting %d streams", cap(s.tiny)), http.StatusServiceUnavailable)
		return
	}

	cmd := exec.CommandContext(r.Context(), s.cfg.FFmpeg, tinyArgs(s.cfg.DVR.SmallStreamURL(number, tinySourceHeight, tinySourceKbps))...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	started := time.Now()
	s.cfg.Logf("tiny: channel %s to %s", number, r.RemoteAddr)
	copyFlushing(w, out)
	// The viewer left, or ffmpeg already ended. Closing the pipe first stops an
	// ffmpeg blocked on writing to it.
	out.Close()
	cmd.Process.Signal(os.Interrupt)
	err = cmd.Wait()
	msg := strings.TrimSpace(stderr.String())
	if len(msg) > 500 {
		msg = msg[len(msg)-500:]
	}
	s.cfg.Logf("tiny: channel %s to %s ended after %s: %v %s", number, r.RemoteAddr, time.Since(started).Round(time.Second), err, msg)
}

// tinyPlaylist lists every channel's tiny stream, for the board to pick from.
func (s *Server) tinyPlaylist(w http.ResponseWriter, r *http.Request) {
	chs, err := s.cfg.DVR.Channels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, ch := range chs {
		name := strings.NewReplacer("\n", " ", ",", " ").Replace(ch.Name)
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-chno=%q tvg-logo=%q,%s\nhttp://%s/tiny/%s.ts\n", ch.Number, ch.Logo, name, r.Host, ch.Number)
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Write([]byte(b.String()))
}
