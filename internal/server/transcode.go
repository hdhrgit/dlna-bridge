package server

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dlna/internal/config"
	"dlna/internal/cut"
)

// agent is what this server calls itself to the DVR.
func (s *Server) agent() string {
	if s.cfg.DVR == nil {
		return ""
	}
	return s.cfg.DVR.UserAgent
}

// evenCeil rounds up to an even number: the DVR's encoder refuses odd heights.
func evenCeil(n int) int { return n + n%2 }

// videoFilter is the picture's way through ffmpeg: no faster than t.FPS, the
// middle cut to the crop shape, then fitted inside the largest size (or
// stretched to it), both sides even. A fit asked for with the stream first
// makes the pixels square, so that it works on the picture's shape as seen.
// Scaled down to a small screen, a 1080i picture's two fields blend into each
// line, so it is not deinterlaced.
func videoFilter(t config.Transcode) string {
	f := []string{fmt.Sprintf(`fps=min(source_fps\,%d)`, t.FPS)}
	w, h := t.MaxWidth, t.MaxHeight
	square := `scale=trunc(iw*sar/2)*2:ih,setsar=1`
	inside := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2", w, h)
	switch t.Fit {
	case config.FitWidth:
		return strings.Join(append(f, square, inside, "setsar=1", "format=yuv420p"), ",")
	case config.FitHeight:
		// The middle in the size's shape, a little wider so that rounding
		// leaves no gap, scaled to the height, then cut to the width. A
		// source narrower than that shape keeps all its width.
		return strings.Join(append(f, square,
			fmt.Sprintf(`crop=min(iw\,ceil(ih*%d/%d/2)*2+2):ih`, w, h),
			fmt.Sprintf("scale=-2:%d", h),
			fmt.Sprintf(`crop=min(iw\,%d):%d,setsar=1`, w, h),
			"format=yuv420p"), ",")
	case config.FitFill:
		return strings.Join(append(f, square,
			fmt.Sprintf(`crop=min(iw\,ih*%[1]d/%[2]d):min(ih\,iw*%[2]d/%[1]d)`, w, h),
			fmt.Sprintf("scale=%d:%d,setsar=1", w, h),
			"format=yuv420p"), ",")
	}
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
// userAgent, if there is one, is what ffmpeg calls itself to the server it reads.
// start, if over 0, is where in the input to begin, in seconds: an input seek,
// which ffmpeg makes with a byte range on a recording.
func ffmpegArgs(t config.Transcode, input string, userAgent string, start float64) []string {
	kbps := func(n int) string { return strconv.Itoa(n) + "k" }
	var params []string
	if t.SingleSlice {
		// zerolatency turns on sliced threads, which splits each frame.
		params = append(params, "sliced-threads=0:slices=1")
	}
	if t.NoDeblock {
		params = append(params, "no-deblock=1")
	}
	x264 := []string{}
	if len(params) > 0 {
		x264 = []string{"-x264-params", strings.Join(params, ":")}
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if userAgent != "" {
		args = append(args, "-user_agent", userAgent)
	}
	if start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', -1, 64))
	}
	args = append(args,
		"-i", input,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", videoFilter(t),
		"-c:v", "libx264", "-profile:v", t.H264Profile,
		"-preset", "veryfast", "-tune", "zerolatency",
	)
	args = append(args, x264...)
	return append(args,
		"-g", strconv.Itoa(t.FPS*2), "-b:v", kbps(t.VideoKbps), "-maxrate", kbps(t.VideoKbps*6/5), "-bufsize", kbps(t.VideoKbps*2),
		"-a53cc", "0", // no caption data for a small decoder to skip
		"-c:a", "aac", "-ac", strconv.Itoa(t.AudioChannels), "-ar", "44100", "-b:a", kbps(t.AudioKbps),
		// Each packet goes out as it is made, not in 32 kB lumps.
		"-flush_packets", "1", "-f", "mpegts", "-muxdelay", "0", "-muxpreload", "0", "pipe:1",
	)
}

// transcode runs ffmpeg for one client and streams what it makes. The stream
// has no length, so no byte offsets; a recording for a client set to seek by
// time starts where it asks with a DLNA time seek (TimeSeekRange.dlna.org).
// length, when known, is the recording's in seconds, for the answer. It stops
// when the client goes. A live channel is kept near its live edge: a client
// that reads slower than it plays skips ahead rather than falling behind.
// With feed, the input is the channel's shared feed rather than input.
func (s *Server) transcode(w http.ResponseWriter, r *http.Request, t config.Transcode, client, what, input string, feed *feedReq, live bool, length float64) {
	w.Header().Set("Content-Type", tsMIME)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	byTime := false
	if _, prof := s.profileFor(r); !live && prof.Seek == config.SeekTime {
		byTime = true
	}
	features := transcodeFeatures
	if byTime {
		features = transcodeTimeFeatures
	}
	w.Header().Set("ContentFeatures.DLNA.ORG", features)
	start := 0.0
	if seek := r.Header.Get("TimeSeekRange.dlna.org"); seek != "" {
		if !byTime {
			// DLNA's answer to a seek that was not offered.
			http.Error(w, "this stream does not seek", http.StatusNotAcceptable)
			return
		}
		var ok bool
		if start, ok = parseNPT(seek); !ok || start > maxStart {
			http.Error(w, "bad TimeSeekRange.dlna.org", http.StatusBadRequest)
			return
		}
		if length > 0 && start >= length {
			http.Error(w, "seek past the end", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		ans := "npt=" + duration(start) + "-"
		if length > 0 {
			ans += duration(length) + "/" + duration(length)
		}
		w.Header().Set("TimeSeekRange.dlna.org", ans)
	}
	w.Header().Set("X-Transcode", describeTranscode(t))
	if r.Method == http.MethodHead {
		return
	}
	ip := s.clientIP(r)
	limit := s.cfg.Settings.MaxTranscodes()
	id, ok := s.jobs.start(limit, client, ip, r.Header.Get("User-Agent"), what)
	if !ok {
		w.Header().Del("X-Transcode")
		http.Error(w, fmt.Sprintf("already converting %d streams", limit), http.StatusServiceUnavailable)
		return
	}
	defer s.jobs.end(id)
	w = &countWriter{w, s.jobs.counter(id)}

	started := time.Now()
	if start > 0 {
		what += fmt.Sprintf(" from %s", time.Duration(start*float64(time.Second)).Round(time.Second))
	}
	s.cfg.Logf("transcode: %s to %s (%s, %s)", what, ip, client, describeTranscode(t))
	args := ffmpegArgs(t, input, s.agent(), start)
	if feed != nil {
		args = ffmpegArgs(t, "pipe:0", "", start)
	}
	if live {
		setSendBuffer(r, liveSendBuffer)
	}
	for try := 1; ; try++ {
		stall := recordingStallLimit
		if live {
			stall = stallLimit
		}
		var in io.Reader
		release := func() {}
		if feed != nil {
			in, release = s.joinFeed(*feed, what)
		}
		sent, msg, err := s.runFFmpeg(w, r, args, in, live, stall, what, func(vw, vh int, fps float64) {
			w.Header().Set("X-Transcode", describeSent(t, vw, vh, fps))
		})
		release()
		s.cfg.Logf("transcode: %s to %s ended after %s: %v %s", what, ip, time.Since(started).Round(time.Second), err, msg)
		// The DVR's stream sometimes fails to start, and ffmpeg is
		// stopped after startLimit. The client has had nothing yet, so it is
		// still waiting: try once more.
		if sent > 0 || try == 2 || r.Context().Err() != nil {
			return
		}
		s.cfg.Logf("transcode: %s gave nothing, trying again", what)
	}
}

// maxStart is the furthest a seek is taken: longer than any recording.
const maxStart = 48 * 3600

// parseNPT reads the start of a DLNA time range, "npt=600.5-" or
// "npt=0:10:00.500-1:00:00", in seconds.
func parseNPT(h string) (float64, bool) {
	v, ok := strings.CutPrefix(strings.TrimSpace(h), "npt=")
	if !ok {
		return 0, false
	}
	v, _, _ = strings.Cut(v, "-")
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) > 3 {
		return 0, false
	}
	var sec float64
	for _, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil || n < 0 || math.IsInf(n, 0) || math.IsNaN(n) {
			return 0, false
		}
		sec = sec*60 + n
	}
	return sec, true
}

// formatLimit is the most output held while looking for the picture's format.
const formatLimit = 512 << 10

// startLimit is how long ffmpeg may take to give its first bytes. A tuner
// takes a few seconds to start; when it fails, ffmpeg can wait on.
const startLimit = 20 * time.Second

// stallLimit is how long a write to a live stream's client may block. A
// device that stops reading but keeps its connection would otherwise hold a
// conversion.
var stallLimit = 20 * time.Second

// recordingStallLimit is the same for a recording, whose ffmpeg runs many
// times faster than the client plays and so waits for it, with the socket
// buffers on the way full. The kernel lets a blocked write go on only once a
// good part of them has been taken, which for a client that reads at the pace
// it plays can be well over stallLimit, as can a proxy's own pauses.
var recordingStallLimit = 5 * time.Minute

// liveBacklog is the most video of a live channel held for a slow client.
// The DVR passes a channel on in bursts of up to 7 s of video, which a client
// with a full buffer has to wait out; beyond that it is behind.
const liveBacklog = 8 * 90000

// liveSendBuffer is the socket buffer for a live conversion, so that what the
// client has not taken waits in the queue, where it can be cut, rather than in
// the kernel.
const liveSendBuffer = 16 << 10

// setSendBuffer sets the request connection's socket send buffer.
func setSendBuffer(r *http.Request, n int) {
	if c, ok := r.Context().Value(connKey{}).(*net.TCPConn); ok {
		c.SetWriteBuffer(n)
	}
}

type connKey struct{}

// ConnContext keeps each connection in its requests' context, for
// http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey{}, c)
}

// runFFmpeg runs one ffmpeg and copies its output to w until either ends. It
// returns the bytes sent and the end of ffmpeg's error output. For a live
// channel the output goes through a queue that is cut when it holds more than
// liveBacklog, so ffmpeg, and the DVR behind it, never wait for the client.
// Otherwise ffmpeg waits for the client, unless a write blocks for stall.
// The start of the output is held until it shows the picture's size and rate,
// which go to format before anything is sent. in, if not nil, is ffmpeg's
// input; the caller stops it once this returns.
func (s *Server) runFFmpeg(w http.ResponseWriter, r *http.Request, args []string, in io.Reader, live bool, stall time.Duration, what string, format func(w, h int, fps float64)) (int64, string, error) {
	cmd := exec.CommandContext(r.Context(), s.cfg.FFmpeg, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	stderr := &tailBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", err
	}
	var stdin io.WriteCloser
	if in != nil {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return 0, "", err
		}
	}
	if err := cmd.Start(); err != nil {
		return 0, "", err
	}
	if stdin != nil {
		go func() {
			io.Copy(stdin, in)
			stdin.Close()
		}()
	}
	var started atomic.Bool
	timer := time.AfterFunc(startLimit, func() {
		if !started.Load() {
			cmd.Process.Kill()
		}
	})
	defer timer.Stop()
	var src io.Reader = out
	copied := make(chan struct{})
	if live {
		q := cut.NewLive(liveBacklog)
		q.OnCut = func(ticks int64) {
			s.cfg.Logf("transcode: %s: the client is behind; left out %.1fs", what, float64(ticks)/90000)
		}
		go func() {
			_, err := io.Copy(q, out)
			q.CloseWithError(err)
			close(copied)
		}()
		src = q
	} else {
		close(copied)
	}
	rc := http.NewResponseController(w)
	buf := make([]byte, 64<<10)
	var sent int64
	var head []byte // the output until its format is known
	for {
		n, rerr := src.Read(buf)
		chunk := buf[:n]
		if n > 0 {
			started.Store(true)
		}
		if format != nil && (n > 0 || len(head) > 0) {
			head = append(head, chunk...)
			vw, vh, fps, ok := cut.VideoFormat(head)
			if !ok && len(head) < formatLimit && rerr == nil {
				continue
			}
			if ok {
				format(vw, vh, fps)
			}
			format, chunk, n = nil, head, len(head)
		}
		if n > 0 {
			rc.SetWriteDeadline(time.Now().Add(stall))
			if _, werr := w.Write(chunk); werr != nil {
				break
			}
			if rc.Flush() != nil {
				break
			}
			sent += int64(n)
		}
		if rerr != nil {
			break
		}
	}
	// The viewer left or stalled, or ffmpeg already ended. Closing the pipe
	// first stops an ffmpeg blocked on writing to it.
	out.Close()
	cmd.Process.Signal(os.Interrupt)
	<-copied
	err = cmd.Wait()
	msg := strings.TrimSpace(stderr.String())
	if len(msg) > 500 {
		msg = msg[len(msg)-500:]
	}
	return sent, msg, err
}

// tailBuffer keeps the last max bytes written to it: the end of ffmpeg's error
// output, which says why it stopped, without holding all of it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
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
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-chno=%q tvg-logo=%q,%s\nhttp://%s/stream/live/%s%s\n", ch.Number, ch.Logo, name, s.hostOf(r), url.PathEscape(ch.Number), suffix)
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Write([]byte(b.String()))
}
