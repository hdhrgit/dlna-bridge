package dvr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Many requests for an empty cache cause one fetch, and all get its value.
func TestCachedOneRefresh(t *testing.T) {
	var c cached[int]
	var fetches atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) (int, error) {
		fetches.Add(1)
		<-release
		return 7, nil
	}
	var wg sync.WaitGroup
	got := make([]int, 20)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = c.get(context.Background(), time.Minute, time.Hour, fetch)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := fetches.Load(); n != 1 {
		t.Errorf("%d fetches", n)
	}
	for _, v := range got {
		if v != 7 {
			t.Fatalf("got %v", got)
		}
	}
}

// A value past its ttl but within maxStale is served at once while one
// refresh runs, so a slow DVR does not hold requests up.
func TestCachedServesStaleWhileRefreshing(t *testing.T) {
	var c cached[int]
	c.get(context.Background(), time.Millisecond, time.Hour, func(context.Context) (int, error) { return 1, nil })
	time.Sleep(5 * time.Millisecond)
	release := make(chan struct{})
	slow := func(context.Context) (int, error) { <-release; return 2, nil }
	start := time.Now()
	v, err := c.get(context.Background(), time.Millisecond, time.Hour, slow)
	if v != 1 || err != nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("stale read: %d %v after %s", v, err, time.Since(start))
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	if v, _ = c.get(context.Background(), time.Minute, time.Hour, slow); v != 2 {
		t.Errorf("after the refresh: %d", v)
	}
}

// Past maxStale the DVR's error is returned, not old data, and health says so.
func TestCachedMaxStale(t *testing.T) {
	var c cached[int]
	c.get(context.Background(), time.Millisecond, 20*time.Millisecond, func(context.Context) (int, error) { return 1, nil })
	down := errors.New("DVR down")
	fail := func(context.Context) (int, error) { return 0, down }

	time.Sleep(5 * time.Millisecond) // stale, within maxStale: still served
	if v, err := c.get(context.Background(), time.Millisecond, 20*time.Millisecond, fail); v != 1 || err != nil {
		t.Fatalf("within maxStale: %d %v", v, err)
	}
	time.Sleep(60 * time.Millisecond) // the refresh failed, and the value is too old now
	if _, err := c.get(context.Background(), time.Millisecond, 20*time.Millisecond, fail); !errors.Is(err, down) {
		t.Errorf("past maxStale: %v", err)
	}
	if at, err, _ := c.health(); at.IsZero() || !errors.Is(err, down) {
		t.Errorf("health: %v %v", at, err)
	}
	// It recovers when the DVR does.
	if v, err := c.get(context.Background(), time.Millisecond, 20*time.Millisecond, func(context.Context) (int, error) { return 3, nil }); v != 3 || err != nil {
		t.Errorf("recovery: %d %v", v, err)
	}
	if _, err, _ := c.health(); err != nil {
		t.Errorf("health after recovery: %v", err)
	}
}

// A request that gives up does not leave the cache stuck.
func TestCachedContext(t *testing.T) {
	var c cached[int]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	block := make(chan struct{})
	defer close(block)
	if _, err := c.get(ctx, time.Minute, time.Hour, func(context.Context) (int, error) { <-block; return 1, nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v", err)
	}
}

// Register sends the name and a stable identity as JSON, and returns the id the
// DVR gives the client.
func TestRegister(t *testing.T) {
	var got struct {
		method, path, ua, ctype string
		body                    map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.ua, got.ctype = r.Method, r.URL.Path, r.Header.Get("User-Agent"), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&got.body)
		io.WriteString(w, `{"alias":"","cid":"abc123","settings":{"client":{},"global":{}}}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL)
	c.UserAgent = "dlna-bridge/1"
	cid, err := c.Register(context.Background(), "DLNA Bridge", "machine-1")
	if err != nil || cid != "abc123" {
		t.Fatalf("cid %q, err %v", cid, err)
	}
	if got.method != "POST" || got.path != "/dvr/clients/register" || got.ctype != "application/json" || got.ua != "dlna-bridge/1" ||
		got.body["hostname"] != "DLNA Bridge" || got.body["machine_id"] != "machine-1" {
		t.Errorf("request: %+v", got)
	}

	// A DVR that refuses, or answers with something else, is an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer bad.Close()
	c2, _ := New(bad.URL)
	if _, err := c2.Register(context.Background(), "x", "y"); err == nil {
		t.Error("a 404 was accepted")
	}
}

func TestRecordingTime(t *testing.T) {
	r := Recording{Duration: 100, Commercials: []float64{10, 20, 50, 70}}
	for _, c := range [][2]float64{{0, 0}, {5, 5}, {10, 20}, {15, 25}, {39.5, 49.5}, {40, 70}, {45, 75}} {
		if got := r.RecordingTime(c[0]); got != c[1] {
			t.Errorf("RecordingTime(%v) = %v, want %v", c[0], got, c[1])
		}
		if back := r.AdFree(c[1]); back != c[0] {
			t.Errorf("AdFree(%v) = %v, want %v", c[1], back, c[0])
		}
	}
}
