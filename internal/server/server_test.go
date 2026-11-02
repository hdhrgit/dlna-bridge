package server

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dlna/internal/config"
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
			{"ID":"21","GroupID":"g2","Airing":{"Title":"Live Show"},"CreatedAt":1793600000}]`,
		"/dvr/groups": `[{"ID":"g1","Name":"Nova","Image":"http://x/nova.jpg"},{"ID":"g2","Name":"Live Show"}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	s := httptest.NewServer(New(Config{Name: "Test DVR", UUID: "u-1", Version: "t", DVR: c, Settings: settings, Bookmarks: b}).Handler())
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
	if br.TotalMatches != 5 || len(d.Containers) != 5 {
		t.Fatalf("root: %+v", d)
	}
	if d.Containers[0].Title != "Live TV" || d.Containers[0].ChildCount != 3 {
		t.Errorf("live folder: %+v", d.Containers[0])
	}
	if d.Containers[1].Title != "Favorites" || d.Containers[1].ChildCount != 1 {
		t.Errorf("favorites folder: %+v", d.Containers[1])
	}
	if d.Containers[2].Title != "Channels by source" || d.Containers[2].ChildCount != 2 {
		t.Errorf("sources folder: %+v", d.Containers[2])
	}
	if d.Containers[3].Title != "Recordings" || d.Containers[3].ChildCount != 3 {
		t.Errorf("recordings folder: %+v", d.Containers[3])
	}
	if d.Containers[4].Title != "Recording now" || d.Containers[4].ChildCount != 1 {
		t.Errorf("recording now folder: %+v", d.Containers[4])
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
