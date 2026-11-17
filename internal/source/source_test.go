package source

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
