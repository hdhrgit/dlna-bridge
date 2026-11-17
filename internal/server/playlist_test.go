package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"dlna/internal/config"
	"dlna/internal/source"
)

// newPlaylistServer serves a playlist of two channels in two groups, one of
// whose streams is live and the other gone, in place of the DVR.
func newPlaylistServer(t *testing.T, start config.Config) (*httptest.Server, *atomic.Int32) {
	ts, err := os.ReadFile("testdata/tall.ts")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	var host *httptest.Server
	host = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list.m3u":
			io.WriteString(w, "#EXTM3U\n"+
				`#EXTINF:-1 tvg-id="one" tvg-chno="11" tvg-logo="`+host.URL+`/one.png" group-title="News",One News`+"\n"+host.URL+"/one.ts\n"+
				`#EXTINF:-1 tvg-chno="12" group-title="Sport",Two Sport`+"\n"+host.URL+"/gone.ts\n")
		case "/one.ts":
			reads.Add(1)
			w.Write(ts)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(host.Close)
	settings, err := config.Open("", start)
	if err != nil {
		t.Fatal(err)
	}
	pl := source.OpenPlaylist(context.Background(), host.URL+"/list.m3u", "", "test", nil)
	srv := New(Config{Name: "Test", UUID: "u-1", Version: "t", Settings: settings, Playlist: pl, Source: host.URL + "/list.m3u", SourceType: source.M3U})
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	return s, &reads
}

func TestPlaylistTree(t *testing.T) {
	s, _ := newPlaylistServer(t, config.Config{Default: config.Profile{Mode: config.ModeProxy}})
	_, root := browse(t, s.URL, "0", "BrowseDirectChildren", 0, 0)
	var ids []string
	for _, c := range root.Containers {
		ids = append(ids, c.ID+"="+c.Title)
	}
	if got := strings.Join(ids, " "); got != "live=Live TV sources=Channels by group listen=Audio listensrc=Audio by group" {
		t.Errorf("root: %s", got)
	}
	_, live := browse(t, s.URL, "live", "BrowseDirectChildren", 0, 0)
	if len(live.Items) != 2 || live.Items[0].ID != "live/one" || live.Items[0].Title != "11 One News" {
		t.Fatalf("Live TV: %+v", live.Items)
	}
	if u := live.Items[0].Res.URL; !strings.HasSuffix(u, "/stream/live/one") {
		t.Errorf("through the bridge: %s", u)
	}
	_, groups := browse(t, s.URL, "sources", "BrowseDirectChildren", 0, 0)
	if len(groups.Containers) != 2 || groups.Containers[0].Title != "News" {
		t.Fatalf("groups: %+v", groups.Containers)
	}
	_, news := browse(t, s.URL, groups.Containers[0].ID, "BrowseDirectChildren", 0, 0)
	if len(news.Items) != 1 || news.Items[0].ID != "srcch/News/one" {
		t.Errorf("News: %+v", news.Items)
	}
	// What only the DVR has is not there.
	for _, id := range []string{"rec", "fav", "movies", "recnow", "others"} {
		if code, _ := soap(t, s.URL, "ContentDirectory", "Browse", "<ObjectID>"+id+"</ObjectID><BrowseFlag>BrowseMetadata</BrowseFlag>"); code != 500 {
			t.Errorf("%s: %d, want a fault", id, code)
		}
	}
	for _, p := range []string{"/channels.m3u", "/stream/rec/1", "/stream/adfree/1"} {
		if resp, err := http.Get(s.URL + p); err != nil || resp.StatusCode != 404 {
			t.Errorf("%s: %v %v", p, resp.Status, err)
		}
	}
	var st state
	resp, err := http.Get(s.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Source == nil || !st.Source.OK || st.Source.Text != "M3U playlist, 2 channels" || st.DVR != "" {
		t.Errorf("state: %+v %+v", st.Source, st.DVR)
	}
}

func TestPlaylistDirect(t *testing.T) {
	s, _ := newPlaylistServer(t, config.Config{Default: config.Profile{Mode: config.ModeDirect}})
	_, live := browse(t, s.URL, "live", "BrowseDirectChildren", 0, 0)
	if len(live.Items) != 2 || !strings.HasSuffix(live.Items[0].Res.URL, "/one.ts") || strings.Contains(live.Items[0].Res.URL, "/stream/") {
		t.Errorf("a direct client plays the playlist's own address: %+v", live.Items)
	}
}

// Through the bridge a channel is relayed from one shared read; a channel
// whose address gives nothing is a 502.
func TestPlaylistRelay(t *testing.T) {
	s, reads := newPlaylistServer(t, config.Config{Default: config.Profile{Mode: config.ModeProxy}})
	read := func() io.ReadCloser {
		resp, err := http.Get(s.URL + "/stream/live/one")
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != videoMIME {
			t.Fatalf("stream: %s %v", resp.Status, resp.Header)
		}
		b := make([]byte, 3*tsPacket)
		if _, err := io.ReadFull(resp.Body, b); err != nil || b[0] != 0x47 {
			t.Fatalf("stream: %v %x", err, b[:4])
		}
		return resp.Body
	}
	a, b := read(), read()
	a.Close()
	b.Close()
	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads of the channel, want 1", n)
	}
	for _, p := range []string{"/stream/live/nosuch", "/stream/audio/nosuch"} {
		if resp, err := http.Get(s.URL + p); err != nil || resp.StatusCode != 404 {
			t.Errorf("%s: %v %v", p, resp.Status, err)
		}
	}
	_, live := browse(t, s.URL, "live", "BrowseDirectChildren", 0, 0)
	resp, err := http.Get(live.Items[1].Res.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a dead channel: %s, want 502", resp.Status)
	}
}

// With no source found yet the root is empty, and nothing else is there.
func TestNoSourceYet(t *testing.T) {
	srv := New(Config{Name: "Test", Source: "192.0.2.20"})
	s := httptest.NewServer(srv.Handler())
	defer s.Close()
	br, root := browse(t, s.URL, "0", "BrowseDirectChildren", 0, 0)
	if br.TotalMatches != 0 || len(root.Containers)+len(root.Items) != 0 {
		t.Errorf("root: %+v", br)
	}
	if code, _ := soap(t, s.URL, "ContentDirectory", "Browse", "<ObjectID>live</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag>"); code != 500 {
		t.Errorf("live: %d", code)
	}
	if resp, err := http.Get(s.URL + "/stream/live/1"); err != nil || resp.StatusCode != 404 {
		t.Errorf("stream: %v %v", resp.Status, err)
	}
	resp, err := http.Get(s.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st state
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Source == nil || st.Source.OK || st.Source.Text != "not found yet" {
		t.Errorf("state: %+v", st.Source)
	}
}
