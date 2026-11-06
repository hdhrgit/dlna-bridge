package server

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"dlna/internal/config"
	"dlna/internal/cut"
	"dlna/internal/dvr"
)

// fakeDVR answers the endpoints the server reads, with the shapes the real
// DVR returned on 2026-11-02.
func fakeDVR(t *testing.T) *httptest.Server {
	now := time.Now().Unix()
	bodies := map[string]string{
		"/api/v1/channels": `[{"id":"cspan.house","name":"C-SPAN House Floor","number":"5000","logo_url":"http://x/logo.png","hd":true,"favorited":true,"source_name":"Tuner A","source_id":"A1"},
			{"id":"hid","name":"Hidden","number":"9","hidden":true},
			{"id":"reuters","name":"Reuters & Co","number":"5002","logo_url":"","hd":true,"source_name":"FAST","source_id":"F1"},
			{"id":"dup","name":"Duplicate","number":"5002","source_name":"Tuner A","source_id":"A1"},
			{"id":"sub","name":"Subchannel","number":"4.1","source_name":"Tuner A","source_id":"A1"}]`,
		"/devices/ANY/guide/now": `[{"Airings":[{"Channel":"5002","Time":` + itoa(now-60) + `,"Duration":3600,"Title":"Evening News"}],"Channel":{"Number":"5002"}}]`,
		"/dvr/files": `[{"ID":"17","GroupID":"g1","Airing":{"Title":"Nova","EpisodeTitle":"Black Holes","SeasonNumber":3,"EpisodeNumber":4,"Image":"http://x/nova.jpg"},"Duration":3605.5,"CreatedAt":1793500000,"PlaybackTime":120,"Completed":true},
			{"ID":18,"GroupID":"g1","Airing":{"Title":"Nova"},"Duration":60,"CreatedAt":1793600000000,"Completed":true,"Processed":true,"FileSize":1000,"Commercials":[10,20,50,70],"PlaybackTime":30},
			{"ID":"19","Airing":{"Title":"Loose"},"Duration":60,"CreatedAt":1793600000000,"Completed":true},
			{"ID":"20","GroupID":"g1","Deleted":true},
			{"ID":"21","GroupID":"g2","Airing":{"Title":"Live Show"},"CreatedAt":1793600000},
			{"ID":"30","GroupID":"movies","Airing":{"Title":"The Caine Mutiny (1954)"},"Duration":7500,"CreatedAt":1790000000,"Completed":true}]`,
		"/dvr/groups": `[{"ID":"g1","Name":"Nova","Image":"http://x/nova.jpg"},{"ID":"g2","Name":"Live Show"},{"ID":"movies","Name":"Movies"}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dvr/files/17/stream.mpg" { // like the DVR: no HEAD, ranges answer 206
			if r.Method == http.MethodHead {
				http.Error(w, "no", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Range", "bytes 0-0/123456")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte{0x47})
			return
		}
		b, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func newTestServer(t *testing.T, proxy bool) *httptest.Server {
	mode := config.ModeDirect
	if proxy {
		mode = config.ModeProxy
	}
	return newServerWith(t, config.Config{Default: config.Profile{Mode: mode, Ads: config.AdsCut}})
}

func newServerWith(t *testing.T, start config.Config) *httptest.Server {
	d := fakeDVR(t)
	c, err := dvr.New(strings.Replace(d.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := OpenBookmarks("")
	settings, err := config.Open("", start)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Name: "Test DVR", UUID: "u-1", Version: "t", DVR: c, Settings: settings, Bookmarks: b})
	// Two hours after the newest recordings in the fake DVR started.
	srv.now = func() time.Time { return time.Unix(1793600000+7200, 0) }
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	return s
}

func soap(t *testing.T, base, service, name, args string) (int, string) {
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:` + name + ` xmlns:u="urn:schemas-upnp-org:service:` + service + `:1">` + args + `</u:` + name + `></s:Body></s:Envelope>`
	req, _ := http.NewRequest("POST", base+"/ctl/"+service, strings.NewReader(body))
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:`+service+`:1#`+name+`"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

type browseResult struct {
	Result         string `xml:"Body>BrowseResponse>Result"`
	NumberReturned int    `xml:"Body>BrowseResponse>NumberReturned"`
	TotalMatches   int    `xml:"Body>BrowseResponse>TotalMatches"`
}

type didlDoc struct {
	Containers []struct {
		ID         string `xml:"id,attr"`
		ChildCount int    `xml:"childCount,attr"`
		Title      string `xml:"title"`
	} `xml:"container"`
	Items []struct {
		ID    string `xml:"id,attr"`
		Title string `xml:"title"`
		Class string `xml:"class"`
		DCM   string `xml:"dcmInfo"`
		Res   struct {
			Protocol string `xml:"protocolInfo,attr"`
			Duration string `xml:"duration,attr"`
			URL      string `xml:",chardata"`
		} `xml:"res"`
	} `xml:"item"`
}

func browse(t *testing.T, base, id, flag string, start, count int) (browseResult, didlDoc) {
	t.Helper()
	code, body := soap(t, base, "ContentDirectory", "Browse",
		"<ObjectID>"+id+"</ObjectID><BrowseFlag>"+flag+"</BrowseFlag><Filter>*</Filter><StartingIndex>"+itoa(int64(start))+"</StartingIndex><RequestedCount>"+itoa(int64(count))+"</RequestedCount><SortCriteria></SortCriteria>")
	if code != 200 {
		t.Fatalf("Browse %s: %d %s", id, code, body)
	}
	var br browseResult
	if err := xml.Unmarshal([]byte(body), &br); err != nil {
		t.Fatal(err)
	}
	var d didlDoc
	if err := xml.Unmarshal([]byte(br.Result), &d); err != nil {
		t.Fatalf("DIDL: %v\n%s", err, br.Result)
	}
	return br, d
}

func TestBrowseRoot(t *testing.T) {
	s := newTestServer(t, false)
	br, d := browse(t, s.URL, "0", "BrowseDirectChildren", 0, 0)
	if br.TotalMatches != 7 || len(d.Containers) != 7 {
		t.Fatalf("root: %+v", d)
	}
	for i, want := range []struct {
		title string
		kids  int
	}{{"Live TV", 3}, {"Favorites", 1}, {"Recent recordings", 3}, {"TV Shows", 3}, {"Movies", 1}, {"Recording now", 1}, {"Channels by source", 2}} {
		if got := d.Containers[i]; got.Title != want.title || got.ChildCount != want.kids {
			t.Errorf("folder %d: %+v, want %s with %d", i, got, want.title, want.kids)
		}
	}
	_, meta := browse(t, s.URL, "0", "BrowseMetadata", 0, 0)
	if len(meta.Containers) != 1 || meta.Containers[0].Title != "Test DVR" {
		t.Errorf("root metadata: %+v", meta)
	}
}

func TestBrowseLive(t *testing.T) {
	s := newTestServer(t, false)
	br, d := browse(t, s.URL, "live", "BrowseDirectChildren", 0, 0)
	if br.TotalMatches != 3 || len(d.Items) != 3 {
		t.Fatalf("live: %+v", d)
	}
	// Ordered by number value, duplicates dropped, now playing in the title.
	if d.Items[0].ID != "live/4.1" || d.Items[2].Title != "5002 Reuters & Co - Evening News" {
		t.Errorf("items: %+v", d.Items)
	}
	// The DVR is at localhost, so the TV gets the address it reached this
	// server on, with the DVR's port.
	if got := d.Items[1].Res.URL; !strings.HasPrefix(got, "http://127.0.0.1:") || !strings.HasSuffix(got, "/devices/ANY/channels/5000/stream.mpg") || strings.HasPrefix(got, s.URL+"/") {
		t.Errorf("stream URL %q, want the DVR's own", got)
	}
	if !strings.Contains(d.Items[1].Res.Protocol, "DLNA.ORG_OP=00") || d.Items[1].Class != classLive {
		t.Errorf("live item: %+v", d.Items[1])
	}

	br, d = browse(t, s.URL, "live", "BrowseDirectChildren", 1, 1)
	if br.TotalMatches != 3 || br.NumberReturned != 1 || d.Items[0].ID != "live/5000" {
		t.Errorf("paging: %+v %+v", br, d)
	}
}

func TestBrowseFavorites(t *testing.T) {
	s := newTestServer(t, false)
	_, d := browse(t, s.URL, "fav", "BrowseDirectChildren", 0, 0)
	if len(d.Items) != 1 || d.Items[0].ID != "fav/5000" {
		t.Fatalf("favorites: %+v", d.Items)
	}
	_, d = browse(t, s.URL, "fav/5000", "BrowseMetadata", 0, 0)
	if len(d.Items) != 1 {
		t.Errorf("favorite metadata: %+v", d)
	}
}

// The movies have a folder of their own, titled by name alone, and are not
// among the shows.
func TestBrowseMovies(t *testing.T) {
	s := newTestServer(t, false)
	_, d := browse(t, s.URL, "movies", "BrowseDirectChildren", 0, 0)
	if len(d.Items) != 1 || d.Items[0].ID != "movies/30" || d.Items[0].Title != "The Caine Mutiny (1954)" {
		t.Fatalf("movies: %+v", d.Items)
	}
	_, meta := browse(t, s.URL, "movies/30", "BrowseMetadata", 0, 0)
	if len(meta.Items) != 1 || meta.Items[0].ID != "movies/30" {
		t.Errorf("movie metadata: %+v", meta)
	}
	// A Samsung's resume point, set from the Movies folder, comes back there.
	if code, body := soap(t, s.URL, "ContentDirectory", "X_SetBookmark", "<CategoryType>0</CategoryType><RID>0</RID><ObjectID>movies/30</ObjectID><PosSecond>600</PosSecond>"); code != 200 {
		t.Fatalf("X_SetBookmark: %d %s", code, body)
	}
	_, meta = browse(t, s.URL, "movies/30", "BrowseMetadata", 0, 0)
	if len(meta.Items) != 1 || !strings.Contains(meta.Items[0].DCM, "BM=600") {
		t.Errorf("movie bookmark: %+v", meta.Items)
	}
}

func TestBrowseRecordings(t *testing.T) {
	s := newTestServer(t, true)
	_, d := browse(t, s.URL, "rec", "BrowseDirectChildren", 0, 0)
	if len(d.Containers) != 3 || d.Containers[0].Title != "Live Show" || d.Containers[1].Title != "Loose" || d.Containers[2].Title != "Nova" || d.Containers[2].ChildCount != 2 {
		t.Fatalf("folders: %+v", d.Containers)
	}
	_, d = browse(t, s.URL, "recgroup/g1", "BrowseDirectChildren", 0, 0)
	if len(d.Items) != 2 {
		t.Fatalf("Nova: %+v", d.Items)
	}
	newest, older := d.Items[0], d.Items[1]
	if newest.ID != "rec/18" || older.Title != "S03E04 Black Holes" {
		t.Errorf("items: %+v", d.Items)
	}
	if older.Res.Duration != "1:00:05.500" || !strings.Contains(older.Res.Protocol, "DLNA.ORG_OP=01") {
		t.Errorf("res: %+v", older.Res)
	}
	if older.Res.URL != s.URL+"/stream/rec/17" {
		t.Errorf("proxy URL %q", older.Res.URL)
	}
	// With marked breaks the recording plays through the bridge without them,
	// and its length and resume point leave them out.
	if newest.Res.URL != s.URL+"/stream/adfree/18" || newest.Res.Duration != "0:00:40.000" {
		t.Errorf("ad-free recording: %+v", newest.Res)
	}
	if !strings.Contains(older.DCM, "BM=120") || !strings.Contains(newest.DCM, "BM=20") {
		t.Errorf("bookmarks from the DVR: %q %q", older.DCM, newest.DCM)
	}

	// A Samsung bookmark replaces the DVR's position.
	if code, body := soap(t, s.URL, "ContentDirectory", "X_SetBookmark", "<CategoryType>0</CategoryType><RID>0</RID><ObjectID>rec/17</ObjectID><PosSecond>300</PosSecond>"); code != 200 {
		t.Fatalf("X_SetBookmark: %d %s", code, body)
	}
	_, d = browse(t, s.URL, "rec/17", "BrowseMetadata", 0, 0)
	if len(d.Items) != 1 || !strings.Contains(d.Items[0].DCM, "BM=300") {
		t.Errorf("bookmark: %+v", d.Items)
	}
}

func TestBrowseErrors(t *testing.T) {
	s := newTestServer(t, false)
	code, body := soap(t, s.URL, "ContentDirectory", "Browse", "<ObjectID>nope</ObjectID><BrowseFlag>BrowseMetadata</BrowseFlag>")
	if code != 500 || !strings.Contains(body, "<errorCode>701</errorCode>") {
		t.Errorf("unknown object: %d %s", code, body)
	}
	code, body = soap(t, s.URL, "ContentDirectory", "Frobnicate", "")
	if code != 500 || !strings.Contains(body, "<errorCode>401</errorCode>") {
		t.Errorf("unknown action: %d %s", code, body)
	}
}

func TestDescriptionAndProtocols(t *testing.T) {
	s := newTestServer(t, false)
	resp, err := http.Get(s.URL + "/description.xml")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var root struct {
		Name string `xml:"device>friendlyName"`
		UDN  string `xml:"device>UDN"`
	}
	if err := xml.Unmarshal(body, &root); err != nil || root.Name != "Test DVR" || root.UDN != "uuid:u-1" {
		t.Errorf("description: %v %+v", err, root)
	}
	for _, p := range []string{"/scpd/ContentDirectory.xml", "/scpd/ConnectionManager.xml"} {
		resp, err := http.Get(s.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		var v struct{}
		if err := xml.NewDecoder(resp.Body).Decode(&v); err != nil || resp.StatusCode != 200 {
			t.Errorf("%s: %d %v", p, resp.StatusCode, err)
		}
		resp.Body.Close()
	}
	code, out := soap(t, s.URL, "ConnectionManager", "GetProtocolInfo", "")
	if code != 200 || !strings.Contains(out, "http-get:*:video/mpeg:*") {
		t.Errorf("GetProtocolInfo: %d %s", code, out)
	}
}

func TestSubscribe(t *testing.T) {
	s := newTestServer(t, false)
	got := make(chan string, 1)
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- r.Method + " " + r.Header.Get("Sid") + " " + string(b)
	}))
	defer cb.Close()
	req, _ := http.NewRequest("SUBSCRIBE", s.URL+"/evt/ContentDirectory", nil)
	req.Header.Set("Callback", "<"+cb.URL+"/>")
	req.Header.Set("NT", "upnp:event")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sid := resp.Header.Get("Sid")
	if resp.StatusCode != 200 || !strings.HasPrefix(sid, "uuid:") {
		t.Fatalf("SUBSCRIBE: %d %q", resp.StatusCode, sid)
	}
	select {
	case ev := <-got:
		if !strings.HasPrefix(ev, "NOTIFY "+sid) || !strings.Contains(ev, "SystemUpdateID") {
			t.Errorf("event: %s", ev)
		}
	case <-time.After(3 * time.Second):
		t.Error("no initial event")
	}
}

func TestPlaylist(t *testing.T) {
	s := newTestServer(t, false)
	resp, err := http.Get(s.URL + "/channels.m3u?profile=Tiny+TV")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "/stream/live/5002?profile=Tiny+TV\n") || !strings.HasPrefix(string(b), "#EXTM3U\n") {
		t.Errorf("playlist:\n%s", b)
	}
}

func TestBrowseSources(t *testing.T) {
	s := newTestServer(t, false)
	_, d := browse(t, s.URL, "sources", "BrowseDirectChildren", 0, 0)
	if len(d.Containers) != 2 || d.Containers[0].Title != "FAST" || d.Containers[1].Title != "Tuner A" || d.Containers[1].ChildCount != 3 {
		t.Fatalf("sources: %+v", d.Containers)
	}
	_, d = browse(t, s.URL, "src/Tuner%20A", "BrowseDirectChildren", 0, 0)
	if len(d.Items) != 3 || d.Items[1].ID != "srcch/Tuner%20A/5000" {
		t.Fatalf("Tuner A: %+v", d.Items)
	}
	// Played from that source, not whichever the DVR picks.
	if !strings.HasSuffix(d.Items[2].Res.URL, "/devices/A1/channels/5002/stream.mpg") {
		t.Errorf("source stream URL %q", d.Items[2].Res.URL)
	}
	_, m := browse(t, s.URL, d.Items[1].ID, "BrowseMetadata", 0, 0)
	if len(m.Items) != 1 {
		t.Errorf("metadata: %+v", m)
	}
}

func TestRecordingNow(t *testing.T) {
	s := newTestServer(t, false)
	_, d := browse(t, s.URL, "recnow", "BrowseDirectChildren", 0, 0)
	if len(d.Items) != 1 || d.Items[0].ID != "recnow/21" || !strings.HasPrefix(d.Items[0].Title, "Live Show ") {
		t.Fatalf("recording now: %+v", d.Items)
	}
	if !strings.HasSuffix(d.Items[0].Res.URL, "/dvr/files/21/stream.mpg") {
		t.Errorf("URL %q", d.Items[0].Res.URL)
	}
}

// Clients get the profile that fits them: the tiny one is converted, another
// plays commercials, and a client asking by name gets that one.
func TestProfiles(t *testing.T) {
	start := config.Config{
		Default: config.Profile{Mode: config.ModeDirect, Ads: config.AdsCut},
		Clients: []config.Client{
			{Name: "Tiny", IPs: []string{"127.0.0.1"}, Profile: config.Profile{Mode: config.ModeTranscode, Ads: config.AdsCut, Transcode: config.DefaultTranscode()}},
			{Name: "Ads", UserAgents: []string{"AdLover"}, Profile: config.Profile{Mode: config.ModeDirect, Ads: config.AdsKeep}},
		},
	}
	s := newServerWith(t, start)
	// Every request here comes from 127.0.0.1, so the first client fits it...
	_, d := browse(t, s.URL, "live", "BrowseDirectChildren", 0, 0)
	if got := d.Items[1].Res.URL; got != strings.TrimPrefix(s.URL, "")+"/stream/live/5000?profile=Tiny" {
		t.Errorf("converted URL %q", got)
	}
	if !strings.Contains(d.Items[1].Res.Protocol, "video/mp2t") || !strings.Contains(d.Items[1].Res.Protocol, "OP=00") {
		t.Errorf("converted protocol %q", d.Items[1].Res.Protocol)
	}
	_, d = browse(t, s.URL, "recgroup/g1", "BrowseDirectChildren", 0, 0)
	if got := d.Items[0].Res.URL; got != s.URL+"/stream/adfree/18?profile=Tiny" {
		t.Errorf("converted recording URL %q", got)
	}
}

func TestProfilesByUserAgent(t *testing.T) {
	start := config.Config{
		Default: config.Profile{Mode: config.ModeDirect, Ads: config.AdsCut},
		Clients: []config.Client{{Name: "Ads", UserAgents: []string{"adlover"}, Profile: config.Profile{Mode: config.ModeDirect, Ads: config.AdsKeep}}},
	}
	s := newServerWith(t, start)
	soapAs := func(ua string) browseResult {
		body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>recgroup/g1</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount><SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
		req, _ := http.NewRequest("POST", s.URL+"/ctl/ContentDirectory", strings.NewReader(body))
		req.Header.Set("User-Agent", ua)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		var br browseResult
		xml.Unmarshal(out, &br)
		return br
	}
	cut, keep := soapAs("LG webOS"), soapAs("AdLover/1.0")
	if !strings.Contains(cut.Result, "/stream/adfree/18") || strings.Contains(keep.Result, "/stream/adfree/18") {
		t.Errorf("ads: cut client\n%s\nkeep client\n%s", cut.Result, keep.Result)
	}
	if !strings.Contains(keep.Result, "/dvr/files/18/stream.mpg") {
		t.Errorf("keep client is not sent to the DVR's file:\n%s", keep.Result)
	}
}

func TestSettingsAPI(t *testing.T) {
	s := newTestServer(t, false)
	put := func(contentType, body string) (int, string) {
		req, _ := http.NewRequest("PUT", s.URL+"/api/config", strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := put("text/plain", `{}`); code != 415 {
		t.Errorf("form post accepted: %d", code)
	}
	if code, body := put("application/json", `{"default":{"mode":"fast"},"max_transcodes":2}`); code != 400 || !strings.Contains(body, "fast") {
		t.Errorf("bad mode: %d %s", code, body)
	}
	if code, body := put("application/json", `{"default":{"mode":"proxy","ads":"keep"},"max_transcodes":3,"clients":[{"name":"A","ips":["10.1.1.1"],"profile":{"mode":"direct","ads":"cut"}}]}`); code != 200 {
		t.Fatalf("save: %d %s", code, body)
	}
	resp, _ := http.Get(s.URL + "/api/state")
	var st struct {
		Config config.Config `json:"config"`
		Name   string        `json:"name"`
		Chans  int           `json:"channels"`
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Config.Default.Mode != "proxy" || len(st.Config.Clients) != 1 || st.Config.MaxTranscodes != 3 || st.Chans != 3 {
		t.Errorf("state: %+v", st)
	}
	resp, _ = http.Get(s.URL + "/")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(page), "DLNA bridge") || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Error("no settings page")
	}
}

// A TV that asks for a stream's headers first is answered as the stream would,
// though the DVR refuses HEAD.
func TestHeadOnStreams(t *testing.T) {
	s := newTestServer(t, true)
	head := func(path string) *http.Response {
		resp, err := http.Head(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	live := head("/stream/live/5000")
	if live.StatusCode != 200 || live.Header.Get("TransferMode.DLNA.ORG") != "Streaming" || live.Header.Get("Content-Type") != "video/mpeg" {
		t.Errorf("live: %d %v", live.StatusCode, live.Header)
	}
	rec := head("/stream/rec/17")
	if rec.StatusCode != 200 || rec.Header.Get("Content-Length") != "123456" || rec.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("recording: %d %v", rec.StatusCode, rec.Header)
	}
}

// A board's settings give one slice per frame, a capped rate, and the middle
// 4:3 of the picture stretched to exactly its size.
func TestFFmpegArgs(t *testing.T) {
	tc := config.DefaultTranscode()
	args := strings.Join(ffmpegArgs(tc, "in", false, "", 0), " ")
	if !strings.Contains(args, `-vf fps=min(source_fps\,24),scale=240:240:force_original_aspect_ratio=decrease,`) || strings.Contains(args, "x264-params") {
		t.Errorf("default: %s", args)
	}
	tc.MaxHeight, tc.ExactSize, tc.Crop, tc.SingleSlice = 192, true, "4:3", true
	args = strings.Join(ffmpegArgs(tc, "in", false, "", 0), " ")
	for _, want := range []string{
		`-vf fps=min(source_fps\,24),crop=min(iw\,ih*4/3):min(ih\,iw*3/4),scale=240:192,setsar=1,format=yuv420p `,
		"-tune zerolatency -x264-params sliced-threads=0:slices=1 -g 48 ",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("board: no %q in %s", want, args)
		}
	}
	if args := strings.Join(ffmpegArgs(tc, "in", true, "", 0), " "); !strings.Contains(args, "-live_start_index -1 -i in ") {
		t.Errorf("live HLS: %s", args)
	}
}

// A recording asked for at ?t= starts there: an input seek, before -i. Without
// it, or with a t that isn't a sensible number of seconds, it starts at the top.
func TestFFmpegStart(t *testing.T) {
	tc := config.DefaultTranscode()
	if args := strings.Join(ffmpegArgs(tc, "in", false, "", 90), " "); !strings.Contains(args, " -ss 90 -i in ") {
		t.Errorf("t=90: %s", args)
	}
	if args := strings.Join(ffmpegArgs(tc, "in", false, "", 12.5), " "); !strings.Contains(args, " -ss 12.5 -i in ") {
		t.Errorf("t=12.5: %s", args)
	}
	if args := strings.Join(ffmpegArgs(tc, "in", false, "", 0), " "); strings.Contains(args, "-ss") {
		t.Errorf("no t: %s", args)
	}
	for q, want := range map[string]float64{
		"": 0, "t=90": 90, "t=12.5": 12.5, "t=0": 0, "t=-5": 0, "t=abc": 0, "t=NaN": 0, "t=Inf": 0, "t=1e9": 0,
	} {
		r := httptest.NewRequest("GET", "/stream/rec/7?profile=esp32&"+q, nil)
		if got := startAt(r); got != want {
			t.Errorf("%q: %v, want %v", q, got, want)
		}
	}
}

// An ffmpeg that ends with nothing, as when the DVR's small stream fails to
// start, is run once more while the client waits.
func TestTranscodeRetries(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nif [ -e " + dir + "/ran ]; then echo picture; else touch " + dir + "/ran; exit 1; fi\n"
	if err := os.WriteFile(ff, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	settings, _ := config.Open("", config.Config{})
	s := New(Config{FFmpeg: ff, Settings: settings})
	w := httptest.NewRecorder()
	s.transcode(w, httptest.NewRequest("GET", "/stream/live/1", nil), config.DefaultTranscode(), "Tiny", "1", "in", false)
	if got := w.Body.String(); got != "picture\n" {
		t.Errorf("got %q", got)
	}
	if len(s.jobs.list()) != 0 {
		t.Error("the job was left running")
	}
}

func TestJobsGroupAndCount(t *testing.T) {
	var j jobs
	a := j.track("recording", "LG", "10.0.0.5", "webos", "Show S01E01")
	b := j.track("recording", "LG", "10.0.0.5", "webos", "Show S01E01") // a TV opens many connections
	c := j.track("live", "", "10.0.0.6", "", "5000 News")
	j.counter(a).Add(1000)
	j.counter(b).Add(500)
	list := j.list()
	if len(list) != 2 || list[0].Conns != 2 || list[0].Bytes != 1500 || list[1].Kind != "live" {
		t.Fatalf("list: %+v", list)
	}
	// Only conversions count against the limit.
	if _, ok := j.start(1, "Tiny", "10.0.0.7", "", "x"); !ok {
		t.Error("a conversion was refused with none running")
	}
	if _, ok := j.start(1, "Tiny", "10.0.0.8", "", "y"); ok {
		t.Error("a second conversion was allowed over the limit")
	}
	j.end(a)
	j.end(b)
	j.end(c)
	if got := j.list(); len(got) != 1 || got[0].Kind != "converted" {
		t.Errorf("after ending: %+v", got)
	}
}

func TestStatusAndLog(t *testing.T) {
	d := fakeDVR(t)
	c, _ := dvr.New(d.URL)
	settings, _ := config.Open("", config.Config{})
	logs := NewLogBuffer(3)
	b, _ := OpenBookmarks("")
	srv := httptest.NewServer(New(Config{Name: "T", UUID: "u", Version: "t", DVR: c, Settings: settings, Bookmarks: b, FFmpeg: "no-such-ffmpeg", Logf: logs.Logf, Logs: logs}).Handler())
	defer srv.Close()

	var st struct {
		Uptime   int64    `json:"uptime_s"`
		FFmpegOK bool     `json:"ffmpeg_ok"`
		FFmpeg   string   `json:"ffmpeg"`
		Location string   `json:"location"`
		Streams  []any    `json:"streams"`
		Activity []string `json:"dvr_activity"`
		Channels int      `json:"channels"`
	}
	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.FFmpegOK || st.FFmpeg == "" || st.Channels != 3 || st.Activity == nil || st.Streams == nil || !strings.HasSuffix(st.Location, "/description.xml") {
		t.Errorf("state: %+v", st)
	}

	// The log keeps the last lines and hands out the new ones.
	for i := 0; i < 5; i++ {
		logs.Logf("line %d", i)
	}
	var lg struct {
		Lines []LogLine `json:"lines"`
		Last  int64     `json:"last"`
	}
	get := func(after string) {
		resp, err := http.Get(srv.URL + "/api/log?after=" + after)
		if err != nil {
			t.Fatal(err)
		}
		lg.Lines = nil
		json.NewDecoder(resp.Body).Decode(&lg)
		resp.Body.Close()
	}
	get("0")
	if len(lg.Lines) < 3 || lg.Lines[len(lg.Lines)-1].Text != "line 4" {
		t.Fatalf("log: %+v", lg)
	}
	get(itoa(lg.Last))
	if len(lg.Lines) != 0 {
		t.Errorf("lines after the last: %+v", lg.Lines)
	}
}

// Recent recordings are those that started within the configured hours,
// newest first, each titled with its show.
func TestRecentRecordings(t *testing.T) {
	s := newTestServer(t, false)
	_, d := browse(t, s.URL, "recent", "BrowseDirectChildren", 0, 0)
	if len(d.Items) != 3 || d.Items[0].ID != "recent/18" && d.Items[0].ID != "recent/19" && d.Items[0].ID != "recent/21" {
		t.Fatalf("last 24 hours: %+v", d.Items)
	}
	for _, it := range d.Items {
		if it.ID == "recent/21" && it.Title != "Live Show 1970-01-01" && !strings.HasPrefix(it.Title, "Live Show") {
			t.Errorf("title %q", it.Title)
		}
		if it.ID == "recent/17" {
			t.Error("a recording from 30 hours ago is listed")
		}
	}
	_, m := browse(t, s.URL, "recent/18", "BrowseMetadata", 0, 0)
	if len(m.Items) != 1 || !strings.Contains(m.Items[0].Res.URL, "/stream/adfree/18") {
		t.Errorf("metadata keeps the profile's treatment of ads: %+v", m.Items)
	}

	// A longer reach takes in the older one, put after the newer.
	wide := newServerWith(t, config.Config{Default: config.Profile{Mode: config.ModeDirect, Ads: config.AdsCut}, RecentHours: 48})
	_, d = browse(t, wide.URL, "recent", "BrowseDirectChildren", 0, 0)
	if len(d.Items) != 4 || d.Items[3].ID != "recent/17" || d.Items[3].Title != "Nova - S03E04 Black Holes" && !strings.HasSuffix(d.Items[3].Title, "S03E04 Black Holes") {
		t.Errorf("last 48 hours: %+v", d.Items)
	}
}

func TestCallbackOnlyToTheSubscriber(t *testing.T) {
	for _, tc := range []struct{ header, from, want string }{
		{"<http://10.0.0.7:49152/evt>", "10.0.0.7", "http://10.0.0.7:49152/evt"},
		{"<http://10.0.0.7/evt>", "10.0.0.7", "http://10.0.0.7/evt"},
		{"<http://10.0.0.9:80/>", "10.0.0.7", ""},                                       // someone else
		{"<http://169.254.169.254/latest/meta-data/>", "10.0.0.7", ""},                  // a metadata service
		{"<http://localhost:8200/ctl/x>", "10.0.0.7", ""},                               // a name, to be looked up
		{"<http://10.0.0.7@evil.example/>", "10.0.0.7", ""},                             // the host is evil.example
		{"<https://10.0.0.7/evt>", "10.0.0.7", ""},                                      // not http
		{"<http://10.0.0.7:99999/evt>", "10.0.0.7", ""},                                 // no such port
		{"<http://10.0.0.9/a><http://10.0.0.7:5/b>", "10.0.0.7", "http://10.0.0.7:5/b"}, // the first that is theirs
		{"<http://[fe80::1]:80/>", "fe80::1", "http://[fe80::1]:80/"},
		{"", "10.0.0.7", ""},
		{"<http://10.0.0.7/>", "not an address", ""},
	} {
		if got := callback(tc.header, tc.from); got != tc.want {
			t.Errorf("callback(%q, %q) = %q, want %q", tc.header, tc.from, got, tc.want)
		}
	}
}

// A subscription naming another host gets its answer, but no request goes there,
// and a redirect from the subscriber's own callback is not followed.
func TestSubscribeDoesNotReachOtherHosts(t *testing.T) {
	s := newTestServer(t, false)
	other, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("no second loopback address:", err)
	}
	hits := make(chan string, 4)
	go http.Serve(other, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits <- r.Method + " " + r.URL.Path }))
	defer other.Close()

	subscribe := func(cb string) *http.Response {
		req, _ := http.NewRequest("SUBSCRIBE", s.URL+"/evt/ContentDirectory", nil)
		req.Header.Set("Callback", "<"+cb+">")
		req.Header.Set("NT", "upnp:event")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := subscribe("http://" + other.Addr().String() + "/evt"); resp.StatusCode != 200 {
		t.Errorf("the subscription was refused: %d", resp.StatusCode)
	}
	// The subscriber's own address, which sends the notice on to the other.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+other.Addr().String()+"/after-redirect", http.StatusFound)
	}))
	defer redirect.Close()
	subscribe(redirect.URL + "/evt")
	select {
	case h := <-hits:
		t.Errorf("a request reached the other host: %s", h)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestPlansAreBoundedAndReplaced(t *testing.T) {
	var p plans
	made := 0
	p.build = func(context.Context, cut.Source, []float64) (*cut.Plan, error) {
		made++
		return &cut.Plan{}, nil
	}
	rec := func(id string, size int64) dvr.Recording {
		return dvr.Recording{ID: dvr.FlexString(id), FileSize: size, Commercials: []float64{1, 2}}
	}
	ctx := context.Background()
	for i := 0; i < 3*maxPlans; i++ {
		p.get(ctx, rec(itoa(int64(i)), 100), nil)
	}
	if len(p.m) != maxPlans || p.lru.Len() != maxPlans {
		t.Fatalf("%d plans kept, want %d", len(p.m), maxPlans)
	}
	// A recent one is kept; the same recording again is not made again.
	before := made
	p.get(ctx, rec(itoa(int64(3*maxPlans-1)), 100), nil)
	if made != before {
		t.Error("a kept plan was made again")
	}
	// A recording whose file changed gets a new plan and does not add an entry.
	p.get(ctx, rec(itoa(int64(3*maxPlans-1)), 200), nil)
	if made != before+1 || len(p.m) != maxPlans {
		t.Errorf("changed recording: made %d more, %d kept", made-before, len(p.m))
	}
	// A failure is not kept.
	p.build = func(context.Context, cut.Source, []float64) (*cut.Plan, error) { return nil, errors.New("no") }
	if _, err := p.get(ctx, rec("new", 1), nil); err == nil {
		t.Fatal("no error")
	}
	if _, ok := p.m["new"]; ok {
		t.Error("a failed plan was kept")
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 10}
	for i := 0; i < 1000; i++ {
		b.Write([]byte("0123456789"))
	}
	b.Write([]byte("end"))
	if got := b.String(); got != "3456789end" || len(b.b) > 10 || cap(b.b) > 4096 {
		t.Errorf("%q (cap %d)", got, cap(b.b))
	}
}

func TestClientIPAndHostBehindProxy(t *testing.T) {
	trusted, err := ParseProxies("127.0.0.0/8, 10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Settings: mustSettings(t, config.Config{}), TrustedProxies: trusted})
	req := func(remote, xff, real, host string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		r.Host = "bridge:8200"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if real != "" {
			r.Header.Set("X-Real-IP", real)
		}
		if host != "" {
			r.Header.Set("X-Forwarded-Host", host)
		}
		return r
	}
	for _, tc := range []struct {
		name string
		r    *http.Request
		ip   string
		host string
	}{
		{"direct, headers ignored", req("192.168.1.50:4000", "9.9.9.9", "8.8.8.8", "evil.example"), "192.168.1.50", "bridge:8200"},
		{"a proxy, one hop", req("127.0.0.1:4000", "192.168.1.50", "", "tv.lan:8080"), "192.168.1.50", "tv.lan:8080"},
		{"two proxies", req("127.0.0.1:4000", "192.168.1.50, 10.0.0.2", "", ""), "192.168.1.50", "bridge:8200"},
		{"a client's own claim is not believed", req("127.0.0.1:4000", "6.6.6.6, 192.168.1.50", "", ""), "192.168.1.50", "bridge:8200"},
		{"real ip when there is no chain", req("127.0.0.1:4000", "", "192.168.1.51", ""), "192.168.1.51", "bridge:8200"},
		{"junk in the chain", req("127.0.0.1:4000", "not-an-ip", "", ""), "127.0.0.1", "bridge:8200"},
	} {
		if got := s.clientIP(tc.r); got != tc.ip {
			t.Errorf("%s: client %q, want %q", tc.name, got, tc.ip)
		}
		if got := s.hostOf(tc.r); got != tc.host {
			t.Errorf("%s: host %q, want %q", tc.name, got, tc.host)
		}
	}
	if _, err := ParseProxies("10.0.0"); err == nil {
		t.Error("a bad proxy address was accepted")
	}
}

func mustSettings(t *testing.T, c config.Config) *config.Store {
	st, err := config.Open("", c)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Through a trusted proxy a TV is matched by its own address and sent URLs on
// the host the proxy was asked for; from anywhere else the headers do nothing.
func TestProfilesBehindProxy(t *testing.T) {
	d := fakeDVR(t)
	c, _ := dvr.New(strings.Replace(d.URL, "127.0.0.1", "localhost", 1))
	b, _ := OpenBookmarks("")
	start := config.Config{
		Default: config.Profile{Mode: config.ModeDirect, Ads: config.AdsCut},
		Clients: []config.Client{{Name: "Tiny", IPs: []string{"192.168.1.77"}, Profile: config.Profile{Mode: config.ModeProxy, Ads: config.AdsCut}}},
	}
	trusted, _ := ParseProxies("127.0.0.1")
	for _, tc := range []struct {
		name    string
		trusted []*net.IPNet
		want    string // the first channel's URL
	}{
		{"trusted", trusted, "http://tv.lan:8080/stream/live/4.1?profile=Tiny"},
		{"not trusted", nil, "/devices/ANY/channels/4.1/stream.mpg"},
	} {
		srv := New(Config{Name: "T", UUID: "u", Version: "t", DVR: c, Settings: mustSettings(t, start), Bookmarks: b, TrustedProxies: tc.trusted})
		ts := httptest.NewServer(srv.Handler())
		body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>live</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>1</RequestedCount><SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>`
		req, _ := http.NewRequest("POST", ts.URL+"/ctl/ContentDirectory", strings.NewReader(body))
		req.Header.Set("X-Forwarded-For", "192.168.1.77")
		req.Header.Set("X-Forwarded-Host", "tv.lan:8080")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ts.Close()
		var br browseResult
		xml.Unmarshal(out, &br)
		if !strings.Contains(br.Result, tc.want) {
			t.Errorf("%s: want %q in\n%s", tc.name, tc.want, br.Result)
		}
	}
}

// The log names the client, not the proxy it came through.
func TestLogNamesTheClient(t *testing.T) {
	trusted, _ := ParseProxies("127.0.0.1")
	var mu sync.Mutex
	var lines []string
	logf := func(format string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	d := fakeDVR(t)
	c, _ := dvr.New(d.URL)
	b, _ := OpenBookmarks("")
	srv := httptest.NewServer(New(Config{Name: "T", UUID: "u", Version: "t", DVR: c, Settings: mustSettings(t, config.Config{}), Bookmarks: b, TrustedProxies: trusted, Logf: logf}).Handler())
	defer srv.Close()
	get := func(forwarded string) {
		req, _ := http.NewRequest("GET", srv.URL+"/description.xml", nil)
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	get("192.168.1.44")
	get("")
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "from 192.168.1.44 via 127.0.0.1") || !strings.HasSuffix(lines[1], "from 127.0.0.1") {
		t.Errorf("log lines: %q", lines)
	}
	// And the SOAP lines say the same.
	lines = nil
	mu.Unlock()
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:GetSortCapabilities xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"></u:GetSortCapabilities></s:Body></s:Envelope>`
	req, _ := http.NewRequest("POST", srv.URL+"/ctl/ContentDirectory", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "192.168.1.44")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	mu.Lock()
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "from 192.168.1.44 via 127.0.0.1") {
		t.Errorf("soap log: %q", lines)
	}
}

// Everything sent to the DVR says what it is, not Go's default; so does the
// command line given to ffmpeg for a stream it reads from the DVR.
func TestUserAgentToTheDVR(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	dvrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("User-Agent")
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/channels":
			io.WriteString(w, `[{"id":"a","name":"A","number":"5"}]`)
		case "/devices/ANY/channels/5/stream.mpg":
			io.WriteString(w, "ts")
		default:
			http.NotFound(w, r)
		}
	}))
	defer dvrSrv.Close()
	c, _ := dvr.New(dvrSrv.URL)
	c.UserAgent = "dlna-bridge/9.9"
	b, _ := OpenBookmarks("")
	start := config.Config{Default: config.Profile{Mode: config.ModeProxy, Ads: config.AdsCut}}
	bridge := httptest.NewServer(New(Config{Name: "T", UUID: "u", Version: "9.9", DVR: c, Settings: mustSettings(t, start), Bookmarks: b}).Handler())
	defer bridge.Close()

	browse(t, bridge.URL, "live", "BrowseDirectChildren", 0, 0) // lists the channels
	resp, err := http.Get(bridge.URL + "/stream/live/5")        // a stream passed through
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	f := c.File(dvr.Recording{ID: "7", FileSize: 10})
	f.ReadAt(context.Background(), 0, 2) // a range of a recording, for the ad cutting; the DVR has no such file, which does not matter here

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/api/v1/channels", "/devices/ANY/channels/5/stream.mpg", "/dvr/files/7/stream.mpg"} {
		if got := seen[path]; got != "dlna-bridge/9.9" {
			t.Errorf("%s asked as %q", path, got)
		}
	}
	if args := strings.Join(ffmpegArgs(config.DefaultTranscode(), "http://dvr/x", false, "dlna-bridge/9.9", 0), " "); !strings.Contains(args, "-user_agent dlna-bridge/9.9 -i http://dvr/x") {
		t.Errorf("ffmpeg: %s", args)
	}
}

// Plans are kept on disk and used again by a server started later, unless the
// recording changed; damaged files are ignored; the directory stays small.
func TestPlansOnDisk(t *testing.T) {
	dir := t.TempDir()
	made := 0
	plan := func() *cut.Plan {
		// Two kept parts and the tables between them, 1 kB in all.
		return &cut.Plan{Parts: []cut.Part{{Off: 0, Len: 188 * 3}, {Data: make([]byte, 188*2), Len: 188 * 2}, {Off: 188 * 10, Len: 188 * 2}}, Size: 188 * 7}
	}
	build := func(context.Context, cut.Source, []float64) (*cut.Plan, error) { made++; return plan(), nil }
	rec := func(size int64) dvr.Recording {
		return dvr.Recording{ID: "42", FileSize: size, Commercials: []float64{1, 2}}
	}
	ctx := context.Background()

	a := &plans{dir: dir, build: build}
	if _, err := a.get(ctx, rec(188*20), nil); err != nil || made != 1 {
		t.Fatalf("first: %v, made %d", err, made)
	}
	// Another server, as after a restart.
	b := &plans{dir: dir, build: build}
	got, err := b.get(ctx, rec(188*20), nil)
	if err != nil || made != 1 || got.Size != 188*7 {
		t.Fatalf("after a restart: %v, made %d, %+v", err, made, got)
	}
	// The recording changed: made again.
	c := &plans{dir: dir, build: build}
	if _, err := c.get(ctx, rec(188*30), nil); err != nil || made != 2 {
		t.Errorf("changed recording: %v, made %d", err, made)
	}
	// A damaged file, or one that does not fit the recording: made again.
	os.WriteFile(a.path("42"), []byte("{not json"), 0o644)
	d := &plans{dir: dir, build: build}
	if _, err := d.get(ctx, rec(188*30), nil); err != nil || made != 3 {
		t.Errorf("damaged file: %v, made %d", err, made)
	}
	// The directory is pruned to the newest files.
	old := maxPlanFiles
	maxPlanFiles = 3
	defer func() { maxPlanFiles = old }()
	e := &plans{dir: dir, build: build}
	for i := 0; i < 8; i++ {
		e.get(ctx, dvr.Recording{ID: dvr.FlexString(itoa(int64(100 + i))), FileSize: 188 * 20, Commercials: []float64{1, 2}}, nil)
	}
	if files, _ := os.ReadDir(dir); len(files) > 3 {
		t.Errorf("%d files kept, want at most 3", len(files))
	}
}

// A recording whose timestamps are no good is refused once, not on every request
// (a TV makes a dozen when it opens one); a network error is tried again.
func TestPlanFailures(t *testing.T) {
	calls := 0
	failWith := errors.New("DVR down")
	p := &plans{build: func(context.Context, cut.Source, []float64) (*cut.Plan, error) {
		calls++
		return nil, failWith
	}}
	var logged []string
	p.logf = func(format string, a ...any) { logged = append(logged, fmt.Sprintf(format, a...)) }
	rec := dvr.Recording{ID: "7", FileSize: 188 * 20, Commercials: []float64{1, 2}}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		p.get(ctx, rec, nil)
	}
	if calls != 3 {
		t.Errorf("a network error was tried %d times, want 3", calls)
	}
	calls, logged, failWith = 0, nil, fmt.Errorf("break at 5s: %w", cut.ErrTimeline)
	p = &plans{logf: p.logf, build: p.build}
	for i := 0; i < 20; i++ {
		if _, err := p.get(ctx, rec, nil); !errors.Is(err, cut.ErrTimeline) {
			t.Fatalf("err %v", err)
		}
	}
	if calls != 1 || len(logged) != 1 {
		t.Errorf("a timeline failure was worked out %d times and logged %d times, want once each", calls, len(logged))
	}
}

// The bridge registers with the DVR under its client name, again after a
// failure and every so often, and the status page says so.
func TestKeepRegistered(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	fail := 2 // the first two attempts fail
	dvrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dvr/clients/register":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(b))
			failing := fail > 0
			fail--
			mu.Unlock()
			if failing {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, `{"cid":"cid-9"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer dvrSrv.Close()
	c, _ := dvr.New(dvrSrv.URL)
	b, _ := OpenBookmarks("")
	var lmu sync.Mutex
	var logs []string
	srv := New(Config{Name: "TV name", UUID: "uuid-1", Version: "t", DVR: c, Settings: mustSettings(t, config.Config{}), Bookmarks: b,
		ClientName: "DLNA Bridge", Logf: func(f string, a ...any) { lmu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); lmu.Unlock() }})
	srv.regEvery, srv.regRetryMin, srv.regRetryMax = 80*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.KeepRegistered(ctx); close(done) }()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		if n >= 5 || time.Now().After(deadline) { // 2 failures, a success, and two renewals
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var st struct {
		Client *registration `json:"dvr_client"`
	}
	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 5 {
		t.Fatalf("%d attempts, want at least 5", len(bodies))
	}
	// The DVR is told the client name, not the DLNA name, and a stable identity.
	for _, body := range bodies {
		if !strings.Contains(body, `"hostname":"DLNA Bridge"`) || !strings.Contains(body, `"machine_id":"uuid-1"`) {
			t.Errorf("registration body %s", body)
		}
	}
	if st.Client == nil || st.Client.Name != "DLNA Bridge" || st.Client.ID != "cid-9" || st.Client.Error != "" {
		t.Errorf("status: %+v", st.Client)
	}
	// One line when it fails, one when it works, not a line for each renewal.
	lmu.Lock()
	defer lmu.Unlock()
	var failed, ok int
	for _, l := range logs {
		if strings.HasPrefix(l, "registering with the DVR") {
			failed++
		}
		if strings.HasPrefix(l, "registered with the DVR") {
			ok++
		}
	}
	if failed != 1 || ok != 1 {
		t.Errorf("log: %d failure lines and %d success lines: %q", failed, ok, logs)
	}
}

// With no client name the bridge does not register at all.
func TestNoRegistrationWithoutAName(t *testing.T) {
	hit := make(chan string, 1)
	dvrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit <- r.URL.Path }))
	defer dvrSrv.Close()
	c, _ := dvr.New(dvrSrv.URL)
	srv := New(Config{Name: "T", UUID: "u", Version: "t", DVR: c, Settings: mustSettings(t, config.Config{})})
	srv.KeepRegistered(context.Background()) // returns at once
	if srv.registration() != nil {
		t.Error("a registration is reported")
	}
	select {
	case p := <-hit:
		t.Errorf("the DVR was asked for %s", p)
	default:
	}
}

// With nothing configured, TVs are sent through the bridge, which works for every
// TV; going straight to the DVR is something to opt into.
func TestDefaultIsThroughTheBridge(t *testing.T) {
	s := newServerWith(t, config.Config{})
	_, d := browse(t, s.URL, "live", "BrowseDirectChildren", 0, 0)
	if len(d.Items) == 0 || !strings.HasPrefix(d.Items[0].Res.URL, s.URL+"/stream/live/") || strings.Contains(d.Items[0].Res.URL, "/devices/ANY/") {
		t.Fatalf("a live channel's URL: %+v", d.Items)
	}
	_, d = browse(t, s.URL, "recgroup/g1", "BrowseDirectChildren", 0, 0)
	for _, it := range d.Items {
		if !strings.HasPrefix(it.Res.URL, s.URL+"/stream/") {
			t.Errorf("recording %s is sent to %s", it.ID, it.Res.URL)
		}
	}
}

// A server made by New must not register in a tight loop: 0.99.2 left the
// intervals at zero and sent dozens of registrations a second.
func TestRegisterIntervalsSet(t *testing.T) {
	s := New(Config{})
	if s.regEvery < time.Minute || s.regRetryMin < time.Second || s.regRetryMax < s.regRetryMin {
		t.Fatalf("intervals %s %s %s", s.regEvery, s.regRetryMin, s.regRetryMax)
	}
	if s.planEvery < time.Minute {
		t.Fatalf("plan ahead every %s", s.planEvery)
	}
}

// Plan ahead leaves the recordings there at the start, plans one that is
// finished and marked later with enough breaks, once, leaves one with few
// breaks to its first play, and asks only for the newest and for the one
// recording, never for the whole library.
func TestPlanAhead(t *testing.T) {
	const six = `[10,20,30,40,50,60,70,80,90,100,110,120]`
	var mu sync.Mutex
	newest := `[{"id":"5","completed":true,"processed":true,"commercials":` + six + `}]`
	var paths []string
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/all":
			if r.URL.Query().Get("limit") == "" {
				t.Errorf("newest without a limit: %s", r.URL)
			}
			io.WriteString(w, newest)
		case "/dvr/files/6":
			io.WriteString(w, `{"ID":"6","Airing":{"Title":"Nova"},"Completed":true,"Processed":true,"FileSize":1000,"Commercials":`+six+`}`)
		case "/dvr/files/8":
			t.Error("asked for a recording with few breaks")
		case "/dvr/files/9":
			io.WriteString(w, `{"ID":"9","Airing":{"Title":"News"},"Completed":true,"Processed":true,"FileSize":1000,"Commercials":[10,20,30,40]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer d.Close()
	c, _ := dvr.New(d.URL)
	settings, _ := config.Open("", config.Config{})
	srv := New(Config{DVR: c, Settings: settings, PlansDir: t.TempDir(), PlanAheadBreaks: 6})
	var planned []string
	srv.plans.build = func(_ context.Context, _ cut.Source, b []float64) (*cut.Plan, error) {
		planned = append(planned, fmt.Sprint(len(b)/2))
		return &cut.Plan{}, nil
	}
	seen := map[string]bool{}
	ctx := context.Background()
	srv.planNewest(ctx, seen, true)
	mu.Lock()
	newest = `[{"id":"6","completed":true,"processed":true,"commercials":` + six + `},
		{"id":"8","completed":true,"processed":true,"commercials":[10,20,30,40]},
		{"id":"7","completed":true,"processed":false},
		{"id":"5","completed":true,"processed":true,"commercials":` + six + `}]`
	mu.Unlock()
	srv.planNewest(ctx, seen, false)
	srv.planNewest(ctx, seen, false)
	if len(planned) != 1 || planned[0] != "6" {
		t.Errorf("planned %v, want recording 6 once", planned)
	}
	for _, p := range paths {
		if p == "/dvr/files" {
			t.Errorf("asked for the whole library: %v", paths)
		}
	}

	// With every client playing the commercials, nothing is planned.
	keep, _ := config.Open("", config.Config{Default: config.Profile{Ads: config.AdsKeep}})
	srv.cfg.Settings = keep
	mu.Lock()
	newest = `[{"id":"6","completed":true,"processed":true,"commercials":[10,30,40,50,60,70,80,90,100,110,120,130]}]`
	mu.Unlock()
	srv.planNewest(ctx, seen, false)
	if len(planned) != 1 {
		t.Errorf("planned %v while no client cuts", planned)
	}
	// A cut-off of 0 plans nothing ahead: PlanAhead returns at once.
	off := New(Config{DVR: c, Settings: settings, PlansDir: t.TempDir()})
	done := make(chan struct{})
	go func() { off.PlanAhead(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("PlanAhead runs with a cut-off of 0")
	}

	// A lower cut-off plans the recording with few breaks too.
	srv.cfg.Settings, srv.cfg.PlanAheadBreaks = settings, 2
	mu.Lock()
	newest = `[{"id":"9","completed":true,"processed":true,"commercials":[10,20,30,40]}]`
	mu.Unlock()
	srv.planNewest(ctx, seen, false)
	if len(planned) != 2 {
		t.Errorf("planned %v with a cut-off of 2", planned)
	}
}

// The status page shows only what the DVR does for the bridge's address, once
// registering has told it that address, and says when the DVR knows that
// address by another client's name.
func TestOwnActivity(t *testing.T) {
	hostname := "DLNA Bridge"
	dvrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dvr/clients/register":
			io.WriteString(w, `{"cid":"cid-9"}`)
		case "/dvr/clients/info":
			io.WriteString(w, `[{"id":"other","hostname":"iPad","local_ip":"10.0.0.8"},{"id":"cid-9","hostname":"`+hostname+`","local_ip":"10.0.0.2"}]`)
		case "/dvr":
			io.WriteString(w, `{"activity":{"6-file-57285-10.0.0.2":"Watching News from DLNA Bridge","6-file-1-10.0.0.8":"Watching Nova from iPad","0-recording":"Recording Nova"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer dvrSrv.Close()
	c, _ := dvr.New(dvrSrv.URL)
	var lmu sync.Mutex
	var logs []string
	srv := New(Config{DVR: c, Settings: mustSettings(t, config.Config{}), ClientName: "DLNA Bridge",
		Logf: func(f string, a ...any) { lmu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); lmu.Unlock() }})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	state := func() (st struct {
		Activity []string `json:"dvr_activity"`
		Own      bool     `json:"dvr_activity_own"`
		Client   struct {
			IP      string `json:"ip"`
			DVRName string `json:"dvr_name"`
		} `json:"dvr_client"`
	}) {
		resp, err := http.Get(ts.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(&st)
		return st
	}
	if st := state(); st.Own || len(st.Activity) != 3 {
		t.Errorf("before registering: %+v", st)
	}
	register := func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { srv.KeepRegistered(ctx); close(done) }()
		for deadline := time.Now().Add(2 * time.Second); srv.registration().ID == "" && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done
	}
	register()
	if st := state(); !st.Own || len(st.Activity) != 1 || st.Activity[0] != "Watching News from DLNA Bridge" || st.Client.IP != "10.0.0.2" {
		t.Errorf("registered: %+v", st)
	}

	// Another client on the bridge's address: the DVR names it, and the log says so.
	hostname = "Home Theater"
	srv.reg = registration{}
	register()
	if st := state(); st.Client.DVRName != "Home Theater" {
		t.Errorf("shared address: %+v", st)
	}
	lmu.Lock()
	defer lmu.Unlock()
	if !strings.Contains(strings.Join(logs, "\n"), `the DVR calls client cid-9 "Home Theater"`) {
		t.Errorf("logs: %q", logs)
	}
}

// A stream request is logged with the range asked for, and again when it ends
// with the status and the size, so that a TV that will not play can be seen.
func TestStreamLog(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	h := logRequests(func(f string, a ...any) { mu.Lock(); logs = append(logs, fmt.Sprintf(f, a...)); mu.Unlock() }, "t",
		func(*http.Request) string { return "10.0.0.7" },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := w.(http.Flusher); !ok {
				t.Error("the writer is not a Flusher")
			}
			w.WriteHeader(http.StatusPartialContent)
			w.Write(make([]byte, 1024))
		}))
	req := httptest.NewRequest("GET", "/stream/adfree/7", nil)
	req.Header.Set("Range", "bytes=0-1023")
	req.Header.Set("User-Agent", "Lavf/58")
	h.ServeHTTP(httptest.NewRecorder(), req)
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 2 || logs[0] != "http: GET /stream/adfree/7 bytes=0-1023 Lavf/58 from 10.0.0.7" || !strings.HasPrefix(logs[1], "http: GET /stream/adfree/7 bytes=0-1023: 206, 1024 bytes in ") {
		t.Errorf("logs: %q", logs)
	}
}

// The page and its data are never kept by a browser cache, and there is a
// favicon.
func TestNoStoreAndFavicon(t *testing.T) {
	s := newTestServer(t, false)
	for _, p := range []string{"/", "/api/log?after=0", "/api/state"} {
		resp, err := http.Get(s.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", p, resp.Header.Get("Cache-Control"))
		}
	}
	resp, err := http.Get(s.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(b, []byte("\x89PNG")) {
		t.Errorf("favicon: %d %s %d bytes", resp.StatusCode, resp.Header.Get("Content-Type"), len(b))
	}
}

// A TV subscribed to ContentDirectory events is told when the DVR's newest
// recordings change, with a higher SystemUpdateID and the next SEQ; a renewal
// of an unknown subscription is refused so the TV subscribes anew; after
// UNSUBSCRIBE no more events come.
func TestChangeEvents(t *testing.T) {
	var mu sync.Mutex
	newest := `[{"id":"5","completed":false}]`
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/v1/all" {
			io.WriteString(w, newest)
			return
		}
		http.NotFound(w, r)
	}))
	defer d.Close()
	type event struct{ seq, body string }
	events := make(chan event, 10)
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		events <- event{r.Header.Get("Seq"), string(b)}
	}))
	defer tv.Close()
	c, _ := dvr.New(d.URL)
	srv := New(Config{DVR: c, Settings: mustSettings(t, config.Config{})})
	srv.watchEvery = 20 * time.Millisecond
	s := httptest.NewServer(srv.Handler())
	defer s.Close()
	sub := func(h map[string]string) *http.Response {
		req, _ := http.NewRequest("SUBSCRIBE", s.URL+"/evt/ContentDirectory", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	resp := sub(map[string]string{"Callback": "<" + tv.URL + "/evt>", "Nt": "upnp:event"})
	sid := resp.Header.Get("Sid")
	first := <-events
	if first.seq != "0" {
		t.Fatalf("first event SEQ %q", first.seq)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.WatchChanges(ctx)
	time.Sleep(60 * time.Millisecond) // the first look only notes what is there
	mu.Lock()
	newest = `[{"id":"5","completed":true}]`
	mu.Unlock()
	select {
	case e := <-events:
		if e.seq != "1" || !strings.Contains(e.body, "<ContainerUpdateIDs>0,") || e.body == first.body {
			t.Errorf("change event: %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event after the recording finished")
	}
	if resp := sub(map[string]string{"Sid": "uuid:unknown"}); resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("renewing an unknown subscription: %d", resp.StatusCode)
	}
	if resp := sub(map[string]string{"Sid": sid}); resp.StatusCode != 200 {
		t.Errorf("renewing: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("UNSUBSCRIBE", s.URL+"/evt/ContentDirectory", nil)
	req.Header.Set("Sid", sid)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
	mu.Lock()
	newest = `[{"id":"6","completed":false},{"id":"5","completed":true}]`
	mu.Unlock()
	select {
	case e := <-events:
		t.Errorf("event after UNSUBSCRIBE: %+v", e)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWatchIntervalSet(t *testing.T) {
	if s := New(Config{}); s.watchEvery < 30*time.Second {
		t.Fatalf("watch every %s", s.watchEvery)
	}
}

// Changes come from the DVR's event stream: a burst of file and show events
// makes one change event to the TV, after the cached lists are dropped. When
// the stream drops, the bridge connects again and catches up on what changed
// meanwhile.
func TestDVREventStream(t *testing.T) {
	var mu sync.Mutex
	newest := `[{"id":"5","completed":false}]`
	files := 0
	lines := make(chan string, 10)
	drop := make(chan struct{})
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/all":
			mu.Lock()
			io.WriteString(w, newest)
			mu.Unlock()
		case "/dvr/files":
			mu.Lock()
			files++
			mu.Unlock()
			io.WriteString(w, `[]`)
		case "/dvr/events/subscribe":
			fl := w.(http.Flusher)
			io.WriteString(w, `{"Type":"hello", "Version":"test"}`+"\n")
			fl.Flush()
			mu.Lock()
			dr := drop
			mu.Unlock()
			for {
				select {
				case l := <-lines:
					io.WriteString(w, l+"\n")
					fl.Flush()
				case <-dr:
					return
				case <-r.Context().Done():
					return
				}
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer d.Close()
	events := make(chan string, 10)
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events <- r.Header.Get("Seq")
	}))
	defer tv.Close()
	c, _ := dvr.New(d.URL)
	srv := New(Config{DVR: c, Settings: mustSettings(t, config.Config{})})
	srv.watchEvery = 50 * time.Millisecond
	s := httptest.NewServer(srv.Handler())
	defer s.Close()
	req, _ := http.NewRequest("SUBSCRIBE", s.URL+"/evt/ContentDirectory", nil)
	req.Header.Set("Callback", "<"+tv.URL+"/evt>")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seq := <-events; seq != "0" {
		t.Fatalf("first SEQ %q", seq)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.cfg.DVR.Recordings(ctx) // cached from now on
	go srv.WatchChanges(ctx)
	time.Sleep(100 * time.Millisecond)

	lines <- `{"Type":"activities.set","Name":"x","Value":"Recording"}` // not a change
	lines <- `{"Type":"files.updated","Name":"5"}`
	lines <- `{"Type":"groups.updated","Name":"g"}`
	select {
	case seq := <-events:
		if seq != "1" {
			t.Errorf("SEQ %q", seq)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no event after files.updated")
	}
	select {
	case seq := <-events:
		t.Errorf("a second event, SEQ %s, for one burst", seq)
	case <-time.After(500 * time.Millisecond):
	}
	srv.cfg.DVR.Recordings(ctx)
	mu.Lock()
	if files != 2 {
		t.Errorf("recordings fetched %d times; the event should drop the cached list", files)
	}
	// The DVR restarts: the stream ends, and something changes meanwhile.
	newest = `[{"id":"6","completed":false},{"id":"5","completed":true}]`
	close(drop)
	drop = make(chan struct{})
	mu.Unlock()
	select {
	case seq := <-events:
		if seq != "2" {
			t.Errorf("SEQ %q after reconnecting", seq)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no event for the change made while the stream was down")
	}
}

// With nothing new, the log answer has an empty list, not null: the page
// added the null to its lines and then failed on every render.
func TestLogNothingNew(t *testing.T) {
	logs := NewLogBuffer(10)
	logs.Logf("one")
	srv := New(Config{Settings: mustSettings(t, config.Config{}), Logs: logs, Logf: logs.Logf})
	s := httptest.NewServer(srv.Handler())
	defer s.Close()
	resp, err := http.Get(s.URL + "/api/log?after=1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"lines":[]`) {
		t.Errorf("answer %s", b)
	}
}

// A recording without a picture from the guide gets the bridge's icon, since
// some TVs leave items without one out of the list.
func TestRecordingArtFallback(t *testing.T) {
	s := newTestServer(t, false)
	_, d := browse(t, s.URL, "rec/19", "BrowseMetadata", 0, 0) // "Loose": no image
	if len(d.Items) != 1 {
		t.Fatalf("items: %+v", d.Items)
	}
	_, raw := soap(t, s.URL, "ContentDirectory", "Browse", "<ObjectID>rec/19</ObjectID><BrowseFlag>BrowseMetadata</BrowseFlag>")
	if !strings.Contains(raw, "/icons/logo120.png") {
		t.Errorf("no fallback picture in %s", raw)
	}
}

// Every top folder has an icon of its own, served as a 160 px PNG; show and
// source folders without a picture of their own get their folder's icon.
func TestFolderIcons(t *testing.T) {
	s := newTestServer(t, false)
	_, raw := soap(t, s.URL, "ContentDirectory", "Browse", "<ObjectID>0</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag>")
	for _, id := range []string{idLive, idFavorites, idRecent, idRecordings, idMovies, idRecNow, idSources} {
		u := "/icons/folder-" + id + ".png"
		if !strings.Contains(raw, u) {
			t.Errorf("no icon for %s in the root listing", id)
		}
		resp, err := http.Get(s.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(resp.Body)
		resp.Body.Close()
		if err != nil || img.Bounds().Dx() != 160 || img.Bounds().Dy() != 160 {
			t.Errorf("%s: %v %v", u, err, img)
		}
	}
	_, raw = soap(t, s.URL, "ContentDirectory", "Browse", "<ObjectID>sources</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag>")
	if !strings.Contains(raw, "/icons/folder-sources.png") {
		t.Error("source folders have no icon")
	}
	if resp, _ := http.Get(s.URL + "/icons/folder-nope.png"); resp.StatusCode != 404 {
		t.Errorf("unknown folder icon: %d", resp.StatusCode)
	}
}

// The DVR's pictures are handed out at the bridge's own address and served
// from there as a JPEG thumbnail; an address not handed out is not fetched.
func TestArtProxy(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 720, 540))
	for i := range big.Pix {
		big.Pix[i] = 0x80
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, big, nil)
	pics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(buf.Bytes())
	}))
	defer pics.Close()
	srv := New(Config{Settings: mustSettings(t, config.Config{})})
	v := view{host: "10.0.0.2:8200"}
	u := srv.artURL(v, pics.URL+"/p123.jpg?w=720")
	if !strings.HasPrefix(u, "http://10.0.0.2:8200/art/") || !strings.HasSuffix(u, ".jpg") {
		t.Fatalf("art address %q", u)
	}
	if got := srv.artURL(v, ""); got != "" {
		t.Errorf("no picture became %q", got)
	}
	s := httptest.NewServer(srv.Handler())
	defer s.Close()
	resp, err := http.Get(s.URL + strings.TrimPrefix(u, "http://10.0.0.2:8200"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(resp.Body)
	resp.Body.Close()
	if err != nil || img.Bounds().Dx() != 160 || img.Bounds().Dy() != 120 {
		t.Fatalf("thumbnail: %v %v", err, img.Bounds())
	}
	if resp, _ := http.Get(s.URL + "/art/0123456789abcdef0123.jpg"); resp.StatusCode != 404 {
		t.Errorf("an address not handed out: %d", resp.StatusCode)
	}
}

// When the show on now changes on a channel, the TVs are told that the
// channel folders changed, once; while it stays the same, nothing is sent.
func TestGuideEvents(t *testing.T) {
	var mu sync.Mutex
	title := "Evening News"
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/devices/ANY/guide/now" {
			io.WriteString(w, `[{"Airings":[{"Channel":"5002","Time":`+itoa(time.Now().Unix()-60)+`,"Duration":3600,"Title":"`+title+`"}],"Channel":{"Number":"5002"}}]`)
			return
		}
		http.NotFound(w, r)
	}))
	defer d.Close()
	bodies := make(chan string, 10)
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
	}))
	defer tv.Close()
	c, _ := dvr.New(d.URL)
	srv := New(Config{DVR: c, Settings: mustSettings(t, config.Config{})})
	srv.guideEvery = 30 * time.Millisecond
	srv.subscribe("uuid:tv", tv.URL+"/evt")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.WatchGuide(ctx)
	select {
	case b := <-bodies:
		t.Fatalf("an event before anything changed: %s", b)
	case <-time.After(150 * time.Millisecond):
	}
	mu.Lock()
	title = "Late Show"
	mu.Unlock()
	select {
	case b := <-bodies:
		if !strings.Contains(b, "<ContainerUpdateIDs>live,") || strings.Contains(b, "recent,") {
			t.Errorf("event %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event after the show on now changed")
	}
	select {
	case b := <-bodies:
		t.Errorf("a second event with nothing changed: %s", b)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestNextGuideLook(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse("15:04:05", s); return v }
	for now, want := range map[string]string{"14:00:00": "14:00:10", "14:00:10": "14:05:10", "14:03:34": "14:05:10", "14:59:59": "15:00:10"} {
		if got := nextGuideLook(at(now), 5*time.Minute).Format("15:04:05"); got != want {
			t.Errorf("after %s: %s, want %s", now, got, want)
		}
	}
}
