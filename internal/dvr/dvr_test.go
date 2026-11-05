package dvr

import (
	"context"
	"errors"
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
