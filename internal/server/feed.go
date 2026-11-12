package server

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"dlna/internal/cut"
	"dlna/internal/dvr"
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

// feedReq names a channel's feed.
type feedReq struct {
	dev, number string
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

// startFeed starts reading a channel from the DVR into a share, with feeds.mu
// held. When the DVR's stream ends, so does the share, and the next client
// starts another.
func (s *Server) startFeed(key string, req feedReq, what string) *feed {
	input := s.cfg.DVR.Base.String() + dvr.LivePath(req.dev, req.number)
	ctx, cancel := context.WithCancel(context.Background())
	f := &feed{share: cut.NewShare(), stop: cancel}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if ua := s.agent(); ua != "" {
		args = append(args, "-user_agent", ua)
	}
	// Copied through ffmpeg, which flags each keyframe where it starts.
	args = append(args, "-i", input,
		"-map", "0:v:0?", "-map", "0:a:0?", "-c", "copy",
		"-flush_packets", "1", "-f", "mpegts", "-muxdelay", "0", "-muxpreload", "0", "pipe:1")
	cmd := exec.CommandContext(ctx, s.cfg.FFmpeg, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second
	stderr := &tailBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		f.share.CloseWithError(err)
		return f
	}
	s.cfg.Logf("feed: %s from the DVR", what)
	started := time.Now()
	go func() {
		_, err := io.Copy(f.share, out)
		f.share.CloseWithError(err)
		werr := cmd.Wait()
		stopped := ctx.Err() != nil
		cancel()
		s.feeds.mu.Lock()
		if s.feeds.m[key] == f {
			delete(s.feeds.m, key)
		}
		s.feeds.mu.Unlock()
		if !stopped {
			s.cfg.Logf("feed: %s ended after %s: %v %s", what, time.Since(started).Round(time.Second), werr, strings.TrimSpace(stderr.String()))
		}
	}()
	return f
}
