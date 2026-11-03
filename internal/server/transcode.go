package server

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"dlna/internal/config"
)

// evenCeil rounds up to an even number: the DVR's encoder refuses odd heights.
func evenCeil(n int) int { return n + n%2 }

// videoFilter is the picture's way through ffmpeg: no faster than t.FPS, the
// middle cut to the crop shape, then fitted inside the largest size (or
// stretched to it), both sides even.
func videoFilter(t config.Transcode) string {
	f := []string{fmt.Sprintf(`fps=min(source_fps\,%d)`, t.FPS)}
	if a, b, ok := t.CropShape(); ok {
		f = append(f, fmt.Sprintf(`crop=min(iw\,ih*%[1]d/%[2]d):min(ih\,iw*%[2]d/%[1]d)`, a, b))
	}
	if t.ExactSize {
		f = append(f, fmt.Sprintf("scale=%d:%d,setsar=1", evenCeil(t.MaxWidth), evenCeil(t.MaxHeight)))
	} else {
		f = append(f, fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2", t.MaxWidth, t.MaxHeight))
	}
	return strings.Join(append(f, "format=yuv420p"), ",")
}

// ffmpegArgs converts input to an MPEG-TS on stdout as the settings say.
func ffmpegArgs(t config.Transcode, input string) []string {
	kbps := func(n int) string { return strconv.Itoa(n) + "k" }
	x264 := []string{}
	if t.SingleSlice {
		// zerolatency turns on sliced threads, which splits each frame.
		x264 = []string{"-x264-params", "sliced-threads=0:slices=1"}
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", input,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", videoFilter(t),
		"-c:v", "libx264", "-profile:v", t.H264Profile,
		"-preset", "veryfast", "-tune", "zerolatency",
	}
	args = append(args, x264...)
	return append(args,
		"-g", strconv.Itoa(t.FPS*2), "-b:v", kbps(t.VideoKbps), "-maxrate", kbps(t.VideoKbps*6/5), "-bufsize", kbps(t.VideoKbps*2),
		"-a53cc", "0", // no caption data for a small decoder to skip
		"-c:a", "aac", "-ac", strconv.Itoa(t.AudioChannels), "-ar", "44100", "-b:a", kbps(t.AudioKbps),
		"-f", "mpegts", "-muxdelay", "0", "pipe:1",
	)
}

// transcode runs ffmpeg for one client and streams what it makes. The stream
// has no length and does not seek. It stops when the client goes.
func (s *Server) transcode(w http.ResponseWriter, r *http.Request, t config.Transcode, client, what, input string) {
	w.Header().Set("Content-Type", tsMIME)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	w.Header().Set("ContentFeatures.DLNA.ORG", "DLNA.ORG_OP=00;DLNA.ORG_CI=1;"+dlnaFlags)
	if r.Method == http.MethodHead {
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	limit := s.cfg.Settings.MaxTranscodes()
	id, ok := s.jobs.start(limit, client, ip, what)
	if !ok {
		http.Error(w, fmt.Sprintf("already converting %d streams", limit), http.StatusServiceUnavailable)
		return
	}
	defer s.jobs.end(id)

	cmd := exec.CommandContext(r.Context(), s.cfg.FFmpeg, ffmpegArgs(t, input)...)
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
	s.cfg.Logf("transcode: %s to %s (%s)", what, ip, client)
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
	s.cfg.Logf("transcode: %s to %s ended after %s: %v %s", what, ip, time.Since(started).Round(time.Second), err, msg)
}

// playlist lists every channel's stream, for players that take an M3U. The
// optional ?profile=<client name> is passed on to each stream.
func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	chs, err := s.cfg.DVR.Channels(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	suffix := ""
	if p := r.URL.Query().Get("profile"); p != "" {
		suffix = "?profile=" + url.QueryEscape(p)
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, ch := range chs {
		name := strings.NewReplacer("\n", " ", ",", " ").Replace(ch.Name)
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-chno=%q tvg-logo=%q,%s\nhttp://%s/stream/live/%s%s\n", ch.Number, ch.Logo, name, r.Host, url.PathEscape(ch.Number), suffix)
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Write([]byte(b.String()))
}
