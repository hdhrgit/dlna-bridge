package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"dlna/internal/config"
)

// pipeListener connects the test's client and server by net.Pipe, which keeps
// nothing in between: each write waits for the client to read it, as it does
// over the network once the buffers on the way are full.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80} }

func (l *pipeListener) client() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, s := net.Pipe()
			select {
			case l.conns <- s:
				return c, nil
			case <-l.done:
				return nil, net.ErrClosed
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}}
}

// recordingServer serves a recording converted by an ffmpeg that writes size
// bytes, with the stall limits set to live and rec.
func recordingServer(t *testing.T, size int, live, rec time.Duration) (*Server, string, *http.Client) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nexec head -c " + strconv.Itoa(size) + " /dev/zero\n"
	if err := os.WriteFile(ff, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	was, wasRec := stallLimit, recordingStallLimit
	stallLimit, recordingStallLimit = live, rec
	t.Cleanup(func() { stallLimit, recordingStallLimit = was, wasRec })
	settings, _ := config.Open("", config.Config{})
	s := New(Config{FFmpeg: ff, Settings: settings, Logf: func(string, ...any) {}})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.transcode(w, r, config.DefaultTranscode(), "Tiny", "recording 1", "in", nil, false, 0)
	}))
	l := newPipeListener()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return s, srv.URL, l.client()
}

// A client that takes a recording at the pace it plays may leave a write
// blocked for longer than a live stream's limit, and still gets all of it.
func TestTranscodeSlowReader(t *testing.T) {
	const size = 1 << 20
	_, url, client := recordingServer(t, size, 300*time.Millisecond, 5*time.Second)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64<<10)
	got := 0
	for {
		n, err := io.ReadFull(resp.Body, buf)
		got += n
		if err != nil {
			break
		}
		if got == 256<<10 {
			time.Sleep(time.Second)
		}
	}
	if got != size {
		t.Errorf("got %d of %d bytes", got, size)
	}
}

// A client that stops reading but keeps its connection is let go after the
// stall limit, and its conversion ends.
func TestTranscodeStoppedReader(t *testing.T) {
	s, url, client := recordingServer(t, 256<<20, 5*time.Second, 300*time.Millisecond)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 4<<10)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(s.jobs.list()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the conversion is still running")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
