package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dlna/internal/config"
	"dlna/internal/source"
)

// fakeMediaServer is a DLNA media server: a folder with a video, a song, a
// picture and an item whose stream is elsewhere.
func fakeMediaServer(t *testing.T) (*httptest.Server, *[]string) {
	var asked []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/desc.xml":
			io.WriteString(w, `<root><device><UDN>uuid:nas-1</UDN><friendlyName>NAS Media</friendlyName><iconList>
				<icon><width>48</width><url>/i48.png</url></icon><icon><width>120</width><url>/i120.png</url></icon></iconList><serviceList>
				<service><serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType><controlURL>/ctl/cd</controlURL></service>
				</serviceList></device></root>`)
			return
		case "/media/v.mkv":
			w.Header().Set("Content-Range", "bytes 0-3/1000")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "abcd")
			return
		case "/ctl/cd":
		default:
			http.NotFound(w, r)
			return
		}
		a, _ := readAction(r.Body)
		asked = append(asked, a.Name+" "+a.Args["ObjectID"]+a.Args["ContainerID"]+" "+a.Args["StartingIndex"]+"/"+a.Args["RequestedCount"])
		host := srv.URL
		items := []string{
			`<item id="v1" parentID="f1" restricted="1"><dc:title>Holiday</dc:title><upnp:class>object.item.videoItem.movie</upnp:class><dc:date>2026-07-01</dc:date>` +
				`<upnp:albumArtURI>` + host + `/art/v1.jpg</upnp:albumArtURI><res protocolInfo="http-get:*:video/x-matroska:DLNA.ORG_OP=01" duration="0:01:30.500" size="1000" resolution="1280x720" bitrate="250000" nrAudioChannels="2" sampleFrequency="48000" colorDepth="24">` + host + `/media/v.mkv</res></item>`,
			`<item id="a1" parentID="f1" restricted="1"><dc:title>Song</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class>` +
				`<res protocolInfo="rtsp-rtp-udp:*:audio/mpeg:*">rtsp://x/a</res><res protocolInfo="http-get:*:audio/mpeg:*" duration="0:03:00">` + host + `/media/a.mp3</res></item>`,
			`<item id="p1" parentID="f1" restricted="1"><dc:title>Photo</dc:title><upnp:class>object.item.imageItem.photo</upnp:class><res protocolInfo="http-get:*:image/jpeg:*">` + host + `/media/p.jpg</res></item>`,
			`<item id="x1" parentID="f1" restricted="1"><dc:title>Elsewhere</dc:title><upnp:class>object.item.videoItem</upnp:class><res protocolInfo="http-get:*:video/mp4:*">http://192.0.2.99/x.mp4</res></item>`,
		}
		var list []string
		total := 0
		switch {
		case a.Name == "GetSearchCapabilities":
			io.WriteString(w, `<s:Envelope><s:Body><u:GetSearchCapabilitiesResponse><SearchCaps>dc:title</SearchCaps></u:GetSearchCapabilitiesResponse></s:Body></s:Envelope>`)
			return
		case a.Args["ObjectID"] == "missing":
			w.WriteHeader(500)
			io.WriteString(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>701</errorCode><errorDescription>No such object</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
			return
		case a.Name == "Search":
			list, total = items[1:2], 1
		case a.Args["BrowseFlag"] == "BrowseMetadata" && a.Args["ObjectID"] == "0":
			list, total = []string{`<container id="0" parentID="-1" childCount="1"><dc:title>root</dc:title></container>`}, 1
		case a.Args["BrowseFlag"] == "BrowseMetadata":
			for _, it := range items {
				if strings.Contains(it, `id="`+a.Args["ObjectID"]+`"`) {
					list, total = []string{it}, 1
				}
			}
		case a.Args["ObjectID"] == "0":
			list, total = []string{`<container id="f1" parentID="2" childCount="4"><dc:title>Media</dc:title></container>`}, 1
		case a.Args["ObjectID"] == "f1":
			start, _ := strconv.Atoi(a.Args["StartingIndex"])
			n, _ := strconv.Atoi(a.Args["RequestedCount"])
			list, total = items[min(start, len(items)):], len(items)
			if n > 0 && n < len(list) {
				list = list[:n]
			}
		}
		didl := didlOpen + strings.Join(list, "") + "</DIDL-Lite>"
		io.WriteString(w, `<s:Envelope><s:Body><u:BrowseResponse><Result>`+escape(didl)+`</Result><NumberReturned>`+strconv.Itoa(len(list))+
			`</NumberReturned><TotalMatches>`+strconv.Itoa(total)+`</TotalMatches></u:BrowseResponse></s:Body></s:Envelope>`)
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func newSourceServer(t *testing.T, ms *httptest.Server, start config.Config) *httptest.Server {
	settings, err := config.Open("", start)
	if err != nil {
		t.Fatal(err)
	}
	d := source.NewServer(ms.URL+"/desc.xml", "test", nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := New(Config{Name: "Channels DVR", NameFromSource: true, UUID: "u-1", Version: "t", Settings: settings, DLNA: d, Source: ms.URL + "/desc.xml", SourceType: source.DLNA})
	go srv.WatchSource(ctx)
	for i := 0; i < 100 && d.Name() == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	return s
}

func TestSourceBrowse(t *testing.T) {
	ms, asked := fakeMediaServer(t)
	s := newSourceServer(t, ms, config.Config{Default: config.Profile{Mode: config.ModeProxy}})
	_, root := browse(t, s.URL, "0", "BrowseMetadata", 0, 0)
	if len(root.Containers) != 1 || root.Containers[0].Title != "NAS Media" {
		t.Errorf("root: %+v", root.Containers)
	}
	_, top := browse(t, s.URL, "0", "BrowseDirectChildren", 0, 0)
	if len(top.Containers) != 1 || top.Containers[0].ID != "f1" || top.Containers[0].ChildCount != 4 || top.Containers[0].Parent != "0" {
		t.Fatalf("top: %+v", top.Containers)
	}
	// Paging goes to the source for a client that sees everything.
	*asked = nil
	br, page := browse(t, s.URL, "f1", "BrowseDirectChildren", 0, 2)
	if br.TotalMatches != 4 || len(page.Items) != 2 || (*asked)[0] != "Browse f1 0/2" {
		t.Errorf("page: %d %+v %v", br.TotalMatches, page.Items, *asked)
	}
	fr, f := browse(t, s.URL, "f1", "BrowseDirectChildren", 0, 0)
	if len(f.Items) != 3 {
		t.Fatalf("an item with its stream elsewhere should be left out: %+v", f.Items)
	}
	v := f.Items[0]
	if v.ID != "v1" || v.Class != "object.item.videoItem.movie" || !strings.HasSuffix(v.Res.URL, "/stream/src/v1") ||
		v.Res.Protocol != "http-get:*:video/x-matroska:DLNA.ORG_OP=01" || v.Res.Duration != "0:01:30.500" {
		t.Errorf("video: %+v", v)
	}
	if !strings.Contains(fr.Result, `duration="0:01:30.500" size="1000" resolution="1280x720" bitrate="250000" nrAudioChannels="2" sampleFrequency="48000">`) {
		t.Errorf("the source's size, resolution, bitrate and sound should be kept: %s", fr.Result)
	}
	if a := f.Items[1]; a.ID != "a1" || !strings.HasSuffix(a.Res.URL, "/stream/src/a1") || a.Res.Protocol != "http-get:*:audio/mpeg:*" {
		t.Errorf("the http stream of the song should be kept: %+v", a)
	}
	if code, body := soap(t, s.URL, "ContentDirectory", "Browse", "<ObjectID>missing</ObjectID><BrowseFlag>BrowseMetadata</BrowseFlag>"); code != 500 || !strings.Contains(body, "<errorCode>701</errorCode>") {
		t.Errorf("the source's fault should be passed on: %d %s", code, body)
	}
	// Streams through the bridge pass byte ranges on, as the source's type.
	req, _ := http.NewRequest("GET", v.Res.URL, nil)
	req.Header.Set("Range", "bytes=0-3")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != "abcd" || resp.Header.Get("Content-Type") != "video/x-matroska" || resp.Header.Get("Content-Range") != "bytes 0-3/1000" {
		t.Errorf("stream: %s %q %v", resp.Status, body, resp.Header)
	}
	if resp, err := http.Get(s.URL + "/stream/src/missing"); err != nil || resp.StatusCode != 404 {
		t.Errorf("a missing item: %v %v", resp.Status, err)
	}
	// Search goes on to the source, which searches.
	code, sr := soap(t, s.URL, "ContentDirectory", "GetSearchCapabilities", "")
	if code != 200 || !strings.Contains(sr, "<SearchCaps>dc:title</SearchCaps>") {
		t.Errorf("caps: %d %s", code, sr)
	}
	code, sr = soap(t, s.URL, "ContentDirectory", "Search", `<ContainerID>0</ContainerID><SearchCriteria>dc:title contains "song"</SearchCriteria><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount>`)
	if code != 200 || !strings.Contains(sr, "Song") {
		t.Errorf("search: %d %s", code, sr)
	}
}

func TestSourceDirectAndKinds(t *testing.T) {
	ms, _ := fakeMediaServer(t)
	s := newSourceServer(t, ms, config.Config{
		Default: config.Profile{Mode: config.ModeDirect},
		Clients: []config.Client{{Name: "speaker", UserAgents: []string{"Speaker"}, Profile: config.Profile{Mode: config.ModeDirect, Kind: config.KindSound}}},
	})
	_, f := browse(t, s.URL, "f1", "BrowseDirectChildren", 0, 0)
	if len(f.Items) != 3 || f.Items[0].Res.URL != ms.URL+"/media/v.mkv" || f.Items[0].Res.Size != "1000" {
		t.Errorf("direct: %+v", f.Items)
	}
	// A client set to sound sees the song alone, counted right.
	body := `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>f1</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><StartingIndex>0</StartingIndex><RequestedCount>1</RequestedCount></u:Browse></s:Body></s:Envelope>`
	req, _ := http.NewRequest("POST", s.URL+"/ctl/ContentDirectory", strings.NewReader(body))
	req.Header.Set("User-Agent", "Speaker/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(out), "<TotalMatches>1</TotalMatches>") || !strings.Contains(string(out), "Song") || strings.Contains(string(out), "Holiday") {
		t.Errorf("sound only: %s", out)
	}
}

// While the source does not answer, the root is empty and streams fail; the
// page says so.
func TestSourceDown(t *testing.T) {
	ms, _ := fakeMediaServer(t)
	s := newSourceServer(t, ms, config.Config{Default: config.Profile{Mode: config.ModeProxy}})
	_, f := browse(t, s.URL, "f1", "BrowseDirectChildren", 0, 0)
	ms.Close()
	br, root := browse(t, s.URL, "0", "BrowseDirectChildren", 0, 0)
	if br.TotalMatches != 0 || len(root.Containers) != 0 {
		t.Errorf("root while down: %+v", br)
	}
	_, meta := browse(t, s.URL, "0", "BrowseMetadata", 0, 0)
	if len(meta.Containers) != 1 || meta.Containers[0].Title != "NAS Media" {
		t.Errorf("root object while down: %+v", meta.Containers)
	}
	resp, err := http.Get(strings.Replace(f.Items[2].Res.URL, "p1", "a1", 1))
	if err != nil || resp.StatusCode != http.StatusBadGateway {
		t.Errorf("stream while down: %v %v", resp.Status, err)
	}
	resp, err = http.Get(s.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var st state
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Source == nil || st.Source.OK || !strings.Contains(st.Source.Detail, "not answering") || st.Name != "NAS Media" {
		t.Errorf("state: %+v %q", st.Source, st.Name)
	}
}

// A converted stream is not the source's file: its size and the rest are not
// passed on.
func TestSourceConvertedHasNoSize(t *testing.T) {
	ms, _ := fakeMediaServer(t)
	s := newSourceServer(t, ms, config.Config{Default: config.Profile{Mode: config.ModeTranscode}})
	br, f := browse(t, s.URL, "f1", "BrowseDirectChildren", 0, 0)
	if len(f.Items) == 0 || f.Items[0].ID != "v1" || f.Items[0].Res.Protocol != transcodeProtocol {
		t.Fatalf("converted: %+v", f.Items)
	}
	for _, a := range resAttrs {
		if strings.Contains(br.Result, " "+a+"=") {
			t.Errorf("%s of a converted stream: %s", a, br.Result)
		}
	}
}

// A tuner's channel is read once for all its clients through the bridge, as
// a playlist's is: each read of it would take a tuner of its own.
func TestSourceChannelSharesOneRead(t *testing.T) {
	ts, err := os.ReadFile("testdata/tall.ts")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	var ms *httptest.Server
	ms = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/desc.xml":
			io.WriteString(w, `<root><device><UDN>uuid:tuner-1</UDN><friendlyName>Tuner</friendlyName><serviceList>
				<service><serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType><controlURL>/ctl/cd</controlURL></service>
				</serviceList></device></root>`)
		case "/auto/v2.1":
			reads.Add(1)
			w.Header().Set("Content-Type", videoMIME)
			w.Write(ts)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/ctl/cd":
			a, _ := readAction(r.Body)
			var list string
			if a.Args["ObjectID"] == "Channels/v2.1" {
				list = `<item id="Channels/v2.1" parentID="Channels" restricted="1"><dc:title>2.1 News</dc:title><upnp:class>object.item.videoItem.videoBroadcast</upnp:class>` +
					`<res protocolInfo="http-get:*:video/mpeg:DLNA.ORG_PN=MPEG_TS_SD_NA_ISO">` + ms.URL + `/auto/v2.1</res></item>`
			}
			io.WriteString(w, `<s:Envelope><s:Body><u:BrowseResponse><Result>`+escape(didlOpen+list+"</DIDL-Lite>")+`</Result><NumberReturned>1</NumberReturned><TotalMatches>1</TotalMatches></u:BrowseResponse></s:Body></s:Envelope>`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ms.Close)
	s := newSourceServer(t, ms, config.Config{Default: config.Profile{Mode: config.ModeProxy}})
	read := func() io.ReadCloser {
		resp, err := http.Get(s.URL + "/stream/src/Channels%2Fv2.1")
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
}
