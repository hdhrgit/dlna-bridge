package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"dlna/internal/dvr"
)

const tsPacket = 188

// Two clients of one channel share one read of it from the DVR, each starting
// at a keyframe; the read stops when the last one leaves.
func TestFeedSharesOneRead(t *testing.T) {
	ts, err := os.ReadFile("testdata/tall.ts")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	var tuner atomic.Value
	gone := make(chan struct{}, 4)
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tuner.Store(r.URL.Path)
		reads.Add(1)
		w.Write(ts)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // live: no end until the reader goes
		gone <- struct{}{}
	}))
	defer d.Close()
	c, err := dvr.New(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{DVR: c})
	first := func(r io.Reader) []byte {
		b := make([]byte, 3*tsPacket)
		if _, err := io.ReadFull(r, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	r1, release1 := s.joinFeed(feedReq{number: "5000"}, "one")
	b1 := first(r1)
	r2, release2 := s.joinFeed(feedReq{number: "5000"}, "two")
	b2 := first(r2)
	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads from the DVR, want 1", n)
	}
	for i, b := range [][]byte{b1, b2} {
		if b[0] != 0x47 || b[1]&0x1f != 0 || b[2] != 0 { // the PAT first
			t.Errorf("reader %d does not start with the PAT: % x", i+1, b[:4])
		}
		if b[2*tsPacket+3]&0x20 == 0 || b[2*tsPacket+5]&0x40 == 0 { // then a keyframe
			t.Errorf("reader %d does not go on with a keyframe", i+1)
		}
	}
	release1()
	select {
	case <-gone:
		t.Error("the read stopped with a reader left")
	case <-time.After(100 * time.Millisecond):
	}
	release2()
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Error("the read goes on with nobody reading")
	}
	if p := tuner.Load(); p != "/devices/ANY/channels/5000/stream.mpg" {
		t.Errorf("read %v", p)
	}
	_, release3 := s.joinFeed(feedReq{dev: "A1", number: "5000"}, "three")
	defer release3()
	for i := 0; i < 100 && reads.Load() < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := reads.Load(); n != 2 {
		t.Errorf("%d reads after the first ended, want 2", n)
	}
	if p := tuner.Load(); p != "/devices/A1/channels/5000/stream.mpg" {
		t.Errorf("read %v for tuner A1", p)
	}
}
