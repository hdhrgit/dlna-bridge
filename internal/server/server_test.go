package server

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"dlna/internal/dvr"
)

// fakeDVR answers the endpoints the server reads, with the shapes the real
// DVR returned on 2026-11-02.
func fakeDVR(t *testing.T) *httptest.Server {
	now := time.Now().Unix()
	bodies := map[string]string{
		"/api/v1/channels": `[{"id":"cspan.house","name":"C-SPAN House Floor","number":"5000","logo_url":"http://x/logo.png","hd":true},
			{"id":"reuters","name":"Reuters & Co","number":"5002","logo_url":"","hd":true},
			{"id":"dup","name":"Duplicate","number":"5002"},
			{"id":"sub","name":"Subchannel","number":"4.1"}]`,
		"/devices/ANY/guide/now": `[{"Airings":[{"Channel":"5002","Time":` + itoa(now-60) + `,"Duration":3600,"Title":"Evening News"}],"Channel":{"Number":"5002"}}]`,
		"/dvr/files": `[{"ID":"17","GroupID":"g1","Airing":{"Title":"Nova","EpisodeTitle":"Black Holes","SeasonNumber":3,"EpisodeNumber":4,"Image":"http://x/nova.jpg"},"Duration":3605.5,"CreatedAt":1793500000,"PlaybackTime":120},
			{"ID":18,"GroupID":"g1","Airing":{"Title":"Nova"},"Duration":60,"CreatedAt":1793600000000,"Completed":true},
			{"ID":"19","Airing":{"Title":"Loose"},"Duration":60,"CreatedAt":1793600000000},
			{"ID":"20","GroupID":"g1","Deleted":true}]`,
		"/dvr/groups": `[{"ID":"g1","Name":"Nova","Image":"http://x/nova.jpg"}]`,
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
	d := fakeDVR(t)
	c, err := dvr.New(strings.Replace(d.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := OpenBookmarks("")
	s := httptest.NewServer(New(Config{Name: "Test DVR", UUID: "u-1", Version: "t", DVR: c, Proxy: proxy, Bookmarks: b}).Handler())
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
	if br.TotalMatches != 2 || len(d.Containers) != 2 {
		t.Fatalf("root: %+v", d)
	}
	if d.Containers[0].Title != "Live TV" || d.Containers[0].ChildCount != 3 {
		t.Errorf("live folder: %+v", d.Containers[0])
	}
	if d.Containers[1].Title != "Recordings" || d.Containers[1].ChildCount != 2 {
		t.Errorf("recordings folder: %+v", d.Containers[1])
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

func TestBrowseRecordings(t *testing.T) {
	s := newTestServer(t, true)
	_, d := browse(t, s.URL, "rec", "BrowseDirectChildren", 0, 0)
	if len(d.Containers) != 2 || d.Containers[0].Title != "Loose" || d.Containers[1].Title != "Nova" || d.Containers[1].ChildCount != 2 {
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
	if !strings.Contains(older.DCM, "BM=120") || newest.DCM != "" {
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

func TestTinyPlaylist(t *testing.T) {
	s := newTestServer(t, false)
	resp, err := http.Get(s.URL + "/tiny/channels.m3u")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "/tiny/5002.ts") || !strings.HasPrefix(string(b), "#EXTM3U\n") {
		t.Errorf("playlist:\n%s", b)
	}
	resp, _ = http.Get(s.URL + "/tiny/9999.ts")
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown channel: %d", resp.StatusCode)
	}
}
