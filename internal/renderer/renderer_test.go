package renderer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A fake renderer: its description, and the actions it was sent.
func fakeTV(t *testing.T, controlHost string) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var got []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/desc.xml" {
			host := controlHost
			if host == "" {
				host = srv.URL
			}
			io.WriteString(w, `<root><device><UDN>uuid:tv-1</UDN><friendlyName>Den</friendlyName><modelName>LG TV</modelName><serviceList>
				<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType><controlURL>`+host+`/avt</controlURL></service>
				<service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType><controlURL>/rc</controlURL></service>
				</serviceList></device></root>`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r.Header.Get("SOAPAction")+" "+string(b))
		mu.Unlock()
		switch {
		case strings.Contains(r.Header.Get("SOAPAction"), "#GetTransportInfo"):
			io.WriteString(w, `<s:Envelope><s:Body><u:GetTransportInfoResponse><CurrentTransportState>PLAYING</CurrentTransportState></u:GetTransportInfoResponse></s:Body></s:Envelope>`)
		case strings.Contains(r.Header.Get("SOAPAction"), "#GetPositionInfo"):
			io.WriteString(w, `<s:Envelope><s:Body><u:R><RelTime>0:10:05</RelTime><TrackDuration>1:00:00.000</TrackDuration></u:R></s:Body></s:Envelope>`)
		case strings.Contains(r.Header.Get("SOAPAction"), "#GetVolume"):
			io.WriteString(w, `<s:Envelope><s:Body><u:R><CurrentVolume>17</CurrentVolume></u:R></s:Body></s:Envelope>`)
		default:
			io.WriteString(w, `<s:Envelope><s:Body/></s:Envelope>`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestRenderer(t *testing.T) {
	tv, got := fakeTV(t, "")
	ctx := context.Background()
	r, err := describe(ctx, tv.URL+"/desc.xml", "127.0.0.1")
	if err != nil || r.Name != "Den" || r.ID != "uuid:tv-1" || !strings.HasSuffix(r.rc, "/rc") {
		t.Fatalf("describe: %+v %v", r, err)
	}
	if err := r.Play(ctx, "http://bridge/stream/rec/7", `<DIDL-Lite>&</DIDL-Lite>`); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*got, "\n")
	for _, want := range []string{"#Stop", "#SetAVTransportURI", "<CurrentURI>http://bridge/stream/rec/7</CurrentURI>", "&lt;DIDL-Lite&gt;&amp;", "#Play"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in %s", want, all)
		}
	}
	r.Seek(ctx, 3725)
	if !strings.Contains(strings.Join(*got, "\n"), "<Target>1:02:05</Target>") {
		t.Error("seek target")
	}
	st, err := r.State(ctx)
	if err != nil || st.State != "PLAYING" || st.Position != 605 || st.Duration != 3600 || st.Volume != 17 {
		t.Errorf("state: %+v %v", st, err)
	}
}

// A description whose control URL is on another address is not used: the
// bridge would send requests wherever a device on the LAN said.
func TestRendererOtherAddress(t *testing.T) {
	tv, _ := fakeTV(t, "http://10.9.9.9:80")
	if _, err := describe(context.Background(), tv.URL+"/desc.xml", "127.0.0.1"); err == nil {
		t.Error("took a control URL on another address")
	}
	if _, err := describe(context.Background(), tv.URL+"/desc.xml", "10.0.0.5"); err == nil {
		t.Error("took a description on another address than the one that answered")
	}
}

func TestPlaysVideo(t *testing.T) {
	for sink, want := range map[string]bool{
		"http-get:*:video/mpeg:*,http-get:*:audio/mpeg:*":                                 true,
		"http-get:*:audio/mpeg:*,http-get:*:video/mpegurl:*,http-get:*:video/x-mpegurl:*": false,
		"http-get:*:audio/vnd.dlna.adts:*":                                                false,
		"http-get:*:*:*":                                                                  true,
	} {
		if got := playsVideo(sink); got != want {
			t.Errorf("%s: %v", sink, got)
		}
	}
}
