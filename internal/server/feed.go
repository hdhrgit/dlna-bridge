package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"dlna/internal/cut"
	"dlna/internal/dvr"
	"dlna/internal/source"
)

// A channel is read from the DVR once, whole, and shared by every conversion
// of it, which each scale it as they need. The DVR takes the reads of one
// channel from one address for one viewer: two of its small streams, even of
// the same size, take each other's segments and both starve. One full read
// also never has to start again for a client that wants a bigger picture.

// feedBehind is the most a conversion may leave unread of its channel's feed
// before it skips to the next keyframe: some 8 s or more of a broadcast
// channel. A conversion only falls behind when the machine is short of time.
const feedBehind = 16 << 20

// feedReq names a channel's feed: from the DVR, or from url, a playlist's
// address for it.
type feedReq struct {
	dev, number, url string
}

type feeds struct {
	mu    sync.Mutex
	m     map[string]*feed // the running feed of each channel
	users map[string]int   // its readers
}

type feed struct {
	share *cut.Share
	stop  context.CancelFunc
}

// joinFeed gives a reader of the channel's feed, which starts at a keyframe,
// and starts the feed if it is not running. release, called when the reader is
// done, stops the feed when nobody reads it any more.
func (s *Server) joinFeed(req feedReq, what string) (r *cut.ShareReader, release func()) {
	key := req.dev + "/" + req.number
	if req.url != "" {
		key = req.url
	}
	s.feeds.mu.Lock()
	defer s.feeds.mu.Unlock()
	if s.feeds.m == nil {
		s.feeds.m, s.feeds.users = map[string]*feed{}, map[string]int{}
	}
	f := s.feeds.m[key]
	if f == nil {
		f = s.startFeed(key, req, what)
		s.feeds.m[key] = f
	}
	s.feeds.users[key]++
	r = f.share.Reader(feedBehind, func() {
		s.cfg.Logf("transcode: %s: the conversion fell behind the channel; skipped ahead", what)
	})
	var once sync.Once
	return r, func() {
		once.Do(func() {
			r.Close()
			s.feeds.mu.Lock()
			defer s.feeds.mu.Unlock()
			if s.feeds.users[key]--; s.feeds.users[key] > 0 {
				return
			}
			delete(s.feeds.users, key)
			if f := s.feeds.m[key]; f != nil {
				f.stop()
				delete(s.feeds.m, key)
			}
		})
	}
}

// startFeed starts reading a channel from the DVR into a share, with
// feeds.mu held. When the DVR's stream ends, so does the share, and the next
// client starts another.
func (s *Server) startFeed(key string, req feedReq, what string) *feed {
	ctx, cancel := context.WithCancel(context.Background())
	f := &feed{share: cut.NewShare(), stop: cancel}
	from := "the DVR"
	if req.url != "" {
		from = "the source"
	}
	s.cfg.Logf("feed: %s from %s", what, from)
	go func() {
		started := time.Now()
		err := s.readFeed(ctx, req, f.share)
		f.share.CloseWithError(err)
		stopped := ctx.Err() != nil
		cancel()
		s.feeds.mu.Lock()
		if s.feeds.m[key] == f {
			delete(s.feeds.m, key)
		}
		s.feeds.mu.Unlock()
		if !stopped {
			s.cfg.Logf("feed: %s ended after %s: %v", what, time.Since(started).Round(time.Second), err)
		}
	}()
	return f
}

// readFeed copies the channel's stream from the DVR, or the playlist's
// address, to w until it ends. An HLS stream is made an MPEG-TS by ffmpeg,
// unconverted: TVs (an LG) do not play HLS.
func (s *Server) readFeed(ctx context.Context, req feedReq, w io.Writer) error {
	u := req.url
	if u == "" {
		u = s.cfg.DVR.Base.String() + dvr.LivePath(req.dev, req.number)
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	s.setAgent(hr)
	// No client timeout: a channel plays as long as it is watched.
	resp, err := http.DefaultTransport.RoundTrip(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(resp.Body, head)
	head = head[:n]
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "mpegurl") || source.IsPlaylist(head) {
		resp.Body.Close()
		return s.remux(ctx, u, w)
	}
	if _, err := w.Write(head); err != nil {
		return err
	}
	_, err = io.Copy(w, resp.Body)
	if err == nil {
		err = io.EOF
	}
	return err
}

// remux has ffmpeg read an HLS stream and write it to w as an MPEG-TS, as it
// is, until it ends.
func (s *Server) remux(ctx context.Context, u string, w io.Writer) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if a := s.agent(); a != "" {
		args = append(args, "-user_agent", a)
	}
	args = append(args, "-i", u, "-sn", "-dn", "-c", "copy", "-f", "mpegts", "pipe:1")
	cmd := exec.CommandContext(ctx, s.cfg.FFmpeg, args...)
	stderr := &tailBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = w, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return io.EOF
}
