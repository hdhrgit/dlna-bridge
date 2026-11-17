package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const list = "\xef\xbb\xbf#EXTM3U x-tvg-url=\"http://example.com/guide.xml\"\n" +
	`#EXTINF:-1 tvg-id="news.example" tvg-name="News HD" tvg-logo="http://example.com/news.png" tvg-chno="7" group-title="News",News, live` + "\n" +
	"http://example.com/news.ts\n" +
	`#EXTINF:-1 tvg-name="Music",Music` + "\n" +
	"#EXTGRP:Radio\n" +
	"music.m3u8\n" +
	`#EXTINF:-1 tvg-id="news.example" tvg-chno="2",News again` + "\n" +
	"http://example.com/news2.ts\n" +
	`#EXTINF:-1,Same stream` + "\n" +
	"http://example.com/news.ts\n" +
	`#EXTINF:-1,Not http` + "\n" +
	"udp://239.0.0.1:1234\n" +
	"http://example.com/no-extinf.ts\n"

func TestParse(t *testing.T) {
	es := Parse([]byte(list), "http://example.com/lists/all.m3u")
	if len(es) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(es), es)
	}
	// By tvg-chno, then the rest after the highest.
	want := []Entry{
		{ID: urlID("http://example.com/news2.ts"), Number: "2", Name: "News again", URL: "http://example.com/news2.ts"},
		{ID: "news.example", Number: "7", Name: "News, live", Logo: "http://example.com/news.png", Group: "News", URL: "http://example.com/news.ts"},
		{ID: urlID("http://example.com/lists/music.m3u8"), Number: "8", Name: "Music", Group: "Radio", URL: "http://example.com/lists/music.m3u8"},
	}
	for i, w := range want {
		if es[i] != w {
			t.Errorf("entry %d:\n got %+v\nwant %+v", i, es[i], w)
		}
	}
}

func TestParseHLS(t *testing.T) {
	hls := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:6\n#EXTINF:6.0,\nseg1.ts\n#EXTINF:6.0,\nseg2.ts\n"
	es := Parse([]byte(hls), "http://example.com/live/channel1.m3u8")
	if len(es) != 1 || es[0].URL != "http://example.com/live/channel1.m3u8" || es[0].Name != "channel1" || es[0].Number != "1" {
		t.Fatalf("an HLS stream should be one channel: %+v", es)
	}
}

func TestDetect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/list.m3u", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, list) })
	mux.HandleFunc("/dvr", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"activity":{},"busy":false,"disk":{"free":1}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html></html>") })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()

	for _, c := range []struct{ addr, kind, where string }{
		{srv.URL + "/list.m3u", M3U, srv.URL + "/list.m3u"},
		{srv.URL + "/", Channels, srv.URL},
		{host, Channels, srv.URL},
	} {
		kind, where, err := Detect(ctx, c.addr, "test")
		if err != nil || kind != c.kind || where != c.where {
			t.Errorf("Detect(%q) = %q, %q, %v; want %q, %q", c.addr, kind, where, err, c.kind, c.where)
		}
	}
	if kind, _, err := Detect(ctx, srv.URL+"/other", "test"); err == nil {
		t.Errorf("a page that is none of them was taken as %q", kind)
	}
}

func TestDVRAddress(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.20":                "http://192.0.2.20:8089",
		"192.0.2.20:8090":           "http://192.0.2.20:8090",
		"dvr.example.com":           "http://dvr.example.com:8089",
		"[2001:db8::1]":             "http://[2001:db8::1]:8089",
		"http://192.0.2.20:8089/":   "http://192.0.2.20:8089",
		"https://dvr.example.com/x": "https://dvr.example.com/x",
	} {
		if got := DVRAddress(in); got != want {
			t.Errorf("DVRAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlaylistKeepsLastGoodCopy(t *testing.T) {
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		io.WriteString(w, list)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cache := CacheFile(dir, srv.URL)
	if filepath.Dir(cache) != dir {
		t.Fatalf("cache %q not in %q", cache, dir)
	}
	p := OpenPlaylist(context.Background(), srv.URL, cache, "test", nil)
	if n, _, err := p.Status(); n != 3 || err != nil {
		t.Fatalf("read %d channels, %v", n, err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("no copy kept: %v", err)
	}
	up = false
	if p.refresh(context.Background()) {
		t.Error("a failed read changed the channels")
	}
	if n, _, err := p.Status(); n != 3 || err == nil {
		t.Errorf("after a failed read: %d channels, %v", n, err)
	}
	// A restart while the host is down has the copy.
	p = OpenPlaylist(context.Background(), srv.URL, cache, "test", nil)
	if _, ok := p.Entry("news.example"); !ok {
		t.Error("the kept copy was not read")
	}
}

// Emby on Windows answers a search from its address with a LOCATION on
// 127.0.0.1 too: only the one on its address is taken, each time.
func TestPickLocation(t *testing.T) {
	ip := "192.0.2.10"
	locs := map[string]string{
		"http://127.0.0.1:8096/dlna/x/description.xml":  ip,
		"http://192.0.2.10:8096/dlna/x/description.xml": ip,
		"http://192.0.2.11:8200/description.xml":        "192.0.2.11",
		"http://192.0.2.10:9000/other/description.xml":  "192.0.2.11",
		"http://[::1]:8096/dlna/x/description.xml":      ip,
		"http://localhost:8096/dlna/x/description.xml":  ip,
	}
	for range 20 { // map order differs each time
		if got := pick(locs, ip); got != "http://192.0.2.10:8096/dlna/x/description.xml" {
			t.Fatalf("pick: %q", got)
		}
	}
	delete(locs, "http://192.0.2.10:8096/dlna/x/description.xml")
	if got := pick(locs, ip); got != "" {
		t.Errorf("none on the address: %q", got)
	}
}

// A server that is not found is logged once, not each minute, and again after
// it was found and is lost.
func TestFindLogsOnce(t *testing.T) {
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/desc.xml":
			io.WriteString(w, `<root><device><friendlyName>NAS</friendlyName><serviceList><service><serviceType>`+ContentDirectory+
				`</serviceType><controlURL>/ctl</controlURL></service></serviceList></device></root>`)
		default:
			io.WriteString(w, `<s:Envelope><s:Body><u:R><SearchCaps></SearchCaps></u:R></s:Body></s:Envelope>`)
		}
	}))
	defer srv.Close()
	var logged []string
	d := NewServer(srv.URL+"/desc.xml", "test", func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	ctx := context.Background()
	for range 3 {
		d.find(ctx)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "not found") {
		t.Fatalf("down: %q", logged)
	}
	up.Store(true)
	if err := d.find(ctx); err != nil {
		t.Fatal(err)
	}
	up.Store(false)
	d.find(ctx)
	d.find(ctx)
	if len(logged) != 3 || !strings.Contains(logged[2], "not found") {
		t.Errorf("lost again: %q", logged)
	}
}
