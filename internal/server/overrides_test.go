package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dlna/internal/config"
	"dlna/internal/dvr"
)

// The two board profiles: 240x144 stretched, and the middle 4:3 at 240x192.
func boardClients() []config.Client {
	esp32 := config.Transcode{MaxWidth: 240, MaxHeight: 144, FPS: 24, VideoKbps: 250, AudioKbps: 64, AudioChannels: 1, H264Profile: "baseline", PreScale: true, ExactSize: true, SingleSlice: true}
	crop := esp32
	crop.MaxHeight, crop.Crop = 192, "4:3"
	return []config.Client{
		{Name: "esp32", Profile: config.Profile{Mode: config.ModeTranscode, Transcode: esp32}},
		{Name: "esp32_4x3", Profile: config.Profile{Mode: config.ModeTranscode, Transcode: crop}},
	}
}

// A board that sends no overrides gets the same ffmpeg command as before
// overrides were added.
func TestBoardArgsUnchanged(t *testing.T) {
	head := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-user_agent", "UA", "-i", "IN", "-map", "0:v:0", "-map", "0:a:0?", "-vf"}
	tail := []string{"-c:v", "libx264", "-profile:v", "baseline", "-preset", "veryfast", "-tune", "zerolatency", "-x264-params", "sliced-threads=0:slices=1", "-g", "48", "-b:v", "250k", "-maxrate", "300k", "-bufsize", "500k", "-a53cc", "0", "-c:a", "aac", "-ac", "1", "-ar", "44100", "-b:a", "64k", "-flush_packets", "1", "-f", "mpegts", "-muxdelay", "0", "-muxpreload", "0", "pipe:1"}
	want := map[string]string{
		"esp32":     `fps=min(source_fps\,24),scale=240:144,setsar=1,format=yuv420p`,
		"esp32_4x3": `fps=min(source_fps\,24),crop=min(iw\,ih*4/3):min(ih\,iw*3/4),scale=240:192,setsar=1,format=yuv420p`,
	}
	settings := mustSettings(t, config.Config{Clients: boardClients()})
	s := New(Config{Settings: settings})
	for name, vf := range want {
		_, prof := s.profileFor(httptest.NewRequest("GET", "/stream/live/5?profile="+name, nil))
		got := ffmpegArgs(prof.Transcode, "IN", "UA", 0)
		exp := append(append(append([]string{}, head...), vf), tail...)
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("%s:\n got %q\nwant %q", name, got, exp)
		}
	}
}

func TestOverrides(t *testing.T) {
	base := boardClients()[0].Profile.Transcode
	lim := config.DefaultLimits()
	for _, c := range []struct {
		query string
		want  string
	}{
		{"", "240x144 fit=scale fps=24 vb=250 ab=64"},
		{"w=320&h=180&fit=scale&fps=24&vb=400&ab=64", "320x180 fit=scale fps=24 vb=400 ab=64"},
		{"w=321&h=181", "320x180 fit=scale fps=24 vb=250 ab=64"},                                                             // down to even
		{"w=9999&h=9999&fps=60&vb=99999&ab=999", "640x480 fit=scale fps=30 vb=1500 ab=128"},                                  // clamped to the limits
		{"w=3&h=1&fps=1&vb=1&ab=1", "16x16 fit=scale fps=1 vb=16 ab=8"},                                                      // and to the least
		{"w=99999999999999999999&vb=99999999999999999999&h=-99999999999999999999", "640x144 fit=scale fps=24 vb=1500 ab=64"}, // too big: the most
		{"w=abc&h=-5&fps=0&vb=1.5&fit=zoom", "240x144 fit=scale fps=24 vb=250 ab=64"},                                        // not values: ignored
		{"w=448&h=252&fit=crop43", "448x252 fit=crop43 fps=24 vb=250 ab=64"},
		{"w=448&h=368&fit=width&fps=15", "448x368 fit=width fps=15 vb=250 ab=64"}, // the most: the source decides
		{"w=448&h=368&fit=height", "448x368 fit=height fps=24 vb=250 ab=64"},
		{"w=448&h=368&fit=fill", "448x368 fit=fill fps=24 vb=250 ab=64"},
	} {
		q, _ := url.ParseQuery(c.query)
		if got := describeTranscode(withOverrides(base, q, lim)); got != c.want {
			t.Errorf("%q: got %q, want %q", c.query, got, c.want)
		}
	}
	// fit=scale on the 4:3 profile drops its crop; crop43 adds one to the plain one.
	crop := boardClients()[1].Profile.Transcode
	if got := withOverrides(crop, url.Values{"fit": {"scale"}}, lim); got.Crop != "" || !got.ExactSize {
		t.Errorf("scale: %+v", got)
	}
	got := withOverrides(config.DefaultTranscode(), url.Values{"w": {"320"}, "h": {"240"}, "fit": {"crop43"}}, lim)
	if vf := videoFilter(got); vf != `fps=min(source_fps\,24),crop=min(iw\,ih*4/3):min(ih\,iw*3/4),scale=320:240,setsar=1,format=yuv420p` {
		t.Errorf("crop43: %s", vf)
	}
	// The fits work on square pixels, from the profile's crop or none.
	sq := `fps=min(source_fps\,15),scale=trunc(iw*sar/2)*2:ih,setsar=1,`
	for fit, want := range map[string]string{
		"width":  sq + `scale=448:368:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2,setsar=1,format=yuv420p`,
		"height": sq + `crop=min(iw\,ceil(ih*448/368/2)*2+2):ih,scale=-2:368,crop=min(iw\,448):368,setsar=1,format=yuv420p`,
		"fill":   sq + `crop=min(iw\,ih*448/368):min(ih\,iw*368/448),scale=448:368,setsar=1,format=yuv420p`,
	} {
		got := withOverrides(crop, url.Values{"w": {"448"}, "h": {"368"}, "fps": {"15"}, "fit": {fit}}, lim)
		if vf := videoFilter(got); vf != want {
			t.Errorf("%s:\n got %s\nwant %s", fit, vf, want)
		}
		// scale and crop43 put the profile's way back.
		if back := withOverrides(got, url.Values{"fit": {"crop43"}}, lim); back.Fit != "" || back.Crop != "4:3" || !back.ExactSize {
			t.Errorf("%s then crop43: %+v", fit, back)
		}
	}
	// Codec, profile and slices stay the base profile's.
	got = withOverrides(base, url.Values{"w": {"320"}, "h": {"180"}}, lim)
	if got.H264Profile != base.H264Profile || got.SingleSlice != base.SingleSlice || got.PreScale != base.PreScale || got.AudioChannels != base.AudioChannels {
		t.Errorf("base settings changed: %+v", got)
	}
	// Smaller limits from the config, an odd size held to the even below.
	small := config.Limits{MaxWidth: 321, MaxHeight: 201, FPS: 20, VideoKbps: 300, AudioKbps: 48}
	q, _ := url.ParseQuery("w=640&h=480&fps=30&vb=1000&ab=128")
	if got := describeTranscode(withOverrides(base, q, small)); got != "320x200 fit=scale fps=20 vb=300 ab=48" {
		t.Errorf("small limits: %s", got)
	}
}

func TestCaps(t *testing.T) {
	get := func(s *httptest.Server) string {
		resp, err := http.Get(s.URL + "/api/caps")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v any
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		return string(b)
	}
	if got := get(newTestServer(t, true)); got != `{"fits":["scale","crop43","width","height","fill"],"max":{"ab":128,"fps":30,"h":480,"vb":1500,"w":640},"overrides":["w","h","fit","fps","vb","ab"],"profiles":["esp32"]}` {
		t.Errorf("default: %s", got)
	}
	s := newServerWith(t, config.Config{Limits: config.Limits{MaxWidth: 448, MaxHeight: 252, FPS: 25, VideoKbps: 800, AudioKbps: 96}})
	if got := get(s); got != `{"fits":["scale","crop43","width","height","fill"],"max":{"ab":96,"fps":25,"h":252,"vb":800,"w":448},"overrides":["w","h","fit","fps","vb","ab"],"profiles":["esp32"]}` {
		t.Errorf("from the config: %s", got)
	}
}

// A converted stream says what it is made of; a stream passed through takes
// no overrides and has no such header.
func TestTranscodeHeader(t *testing.T) {
	s := newServerWith(t, config.Config{Default: config.Profile{Mode: config.ModeProxy}, Clients: boardClients()})
	head := func(path string) http.Header {
		resp, err := http.Head(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header
	}
	for path, want := range map[string]string{
		"/stream/rec/17?profile=esp32":                                   "240x144 fit=scale fps=24 vb=250 ab=64",
		"/stream/rec/17?profile=esp32_4x3":                               "240x192 fit=crop43 fps=24 vb=250 ab=64",
		"/stream/rec/17?profile=esp32&w=320&h=180&fps=24&vb=400&ab=64":   "320x180 fit=scale fps=24 vb=400 ab=64",
		"/stream/adfree/17?profile=esp32&w=448&h=252&fit=crop43&vb=5000": "448x252 fit=crop43 fps=24 vb=1500 ab=64",
		"/stream/rec/17?w=320&h=180":                                     "",
	} {
		if got := head(path).Get("X-Transcode"); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
	// With no esp32 client in the config, the built-in one serves the boards.
	s = newServerWith(t, config.Config{Default: config.Profile{Mode: config.ModeProxy}})
	for path, want := range map[string]string{
		"/stream/rec/17?profile=esp32":                        "240x144 fit=scale fps=24 vb=250 ab=64",
		"/stream/rec/17?profile=esp32&w=320&h=240&fit=crop43": "320x240 fit=crop43 fps=24 vb=250 ab=64",
		"/stream/rec/17?profile=esp32&w=9999&h=9999&vb=9999":  "640x480 fit=scale fps=24 vb=1500 ab=64",
	} {
		if got := head(path).Get("X-Transcode"); got != want {
			t.Errorf("%s: got %q, want %q", path, got, want)
		}
	}
}

// A stream with a fit that depends on the source says the size and rate in
// what it sends; a HEAD, with nothing read, the most.
func TestFitHeaderSent(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	ts, err := filepath.Abs("testdata/tall.ts") // 208x368 at 15 fps
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ff, []byte("#!/bin/sh\ncat "+ts+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := fakeDVR(t)
	c, err := dvr.New(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(New(Config{DVR: c, FFmpeg: ff, Settings: mustSettings(t, config.Config{Clients: boardClients()}), Logf: func(string, ...any) {}}).Handler())
	defer s.Close()
	want, _ := os.ReadFile(ts)
	for method, header := range map[string]string{
		"GET":  "208x368 fit=height fps=15 vb=250 ab=64",
		"HEAD": "448x368 fit=height fps=24 vb=250 ab=64",
	} {
		req, _ := http.NewRequest(method, s.URL+"/stream/rec/17?profile=esp32&w=448&h=368&fit=height", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := resp.Header.Get("X-Transcode"); got != header {
			t.Errorf("%s: header %q, want %q", method, got, header)
		}
		if method == "GET" && !bytes.Equal(body, want) {
			t.Errorf("GET: %d bytes, want %d", len(body), len(want))
		}
	}
}

// Each client's conversion of a live channel's shared read has the client's
// size and rates. The header goes with a
// stream, not with a refusal.
func TestLiveOverrides(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ff, []byte("#!/bin/sh\necho \"$@\" >> "+dir+"/args\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := fakeDVR(t)
	c, err := dvr.New(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{DVR: c, FFmpeg: ff, Settings: mustSettings(t, config.Config{MaxTranscodes: 1, Clients: boardClients()}), Logf: func(string, ...any) {}})
	s := httptest.NewServer(srv.Handler())
	defer s.Close()
	get := func(query string) (*http.Response, string) {
		os.Remove(filepath.Join(dir, "args"))
		resp, err := http.Get(s.URL + "/stream/live/5000?" + query)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		args, _ := os.ReadFile(filepath.Join(dir, "args"))
		return resp, string(args)
	}
	for query, want := range map[string][2]string{
		"profile=esp32":                           {"240x144 fit=scale fps=24 vb=250 ab=64", "scale=240:144,setsar=1,format=yuv420p -c:v libx264 -profile:v baseline -preset veryfast -tune zerolatency -x264-params sliced-threads=0:slices=1 -g 48 -b:v 250k -maxrate 300k -bufsize 500k -a53cc 0 -c:a aac -ac 1 -ar 44100 -b:a 64k "},
		"profile=esp32_4x3":                       {"240x192 fit=crop43 fps=24 vb=250 ab=64", "scale=240:192,setsar=1,format=yuv420p "},
		"profile=esp32&w=320&h=181&vb=400&ab=128": {"320x180 fit=scale fps=24 vb=400 ab=128", "scale=320:180,setsar=1,format=yuv420p -c:v libx264 -profile:v baseline -preset veryfast -tune zerolatency -x264-params sliced-threads=0:slices=1 -g 48 -b:v 400k -maxrate 480k -bufsize 800k -a53cc 0 -c:a aac -ac 1 -ar 44100 -b:a 128k "},
	} {
		resp, args := get(query)
		if got := resp.Header.Get("X-Transcode"); got != want[0] {
			t.Errorf("%s: header %q, want %q", query, got, want[0])
		}
		if !strings.Contains(args, "-i pipe:0 ") || !strings.Contains(args, want[1]) {
			t.Errorf("%s: args %s", query, args)
		}
	}
	id, _ := srv.jobs.start(1, "other", "", "", "busy")
	defer srv.jobs.end(id)
	if resp, _ := get("profile=esp32"); resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Transcode") != "" {
		t.Errorf("over the limit: %d %v", resp.StatusCode, resp.Header)
	}
}

// Limits missing from a config file are the defaults, each on its own; ones
// out of range are refused like any other setting.
func TestLimitsInConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"override_limits":{"max_width":800}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	partial, err := config.Open(file, config.Config{})
	if err != nil {
		t.Fatalf("one limit set: %v", err)
	}
	if got, want := partial.Limits(), (config.Limits{MaxWidth: 800, MaxHeight: 480, FPS: 30, VideoKbps: 1500, AudioKbps: 128}); got != want {
		t.Errorf("one limit set: %+v", got)
	}
	st := mustSettings(t, config.Config{})
	if got := st.Limits(); got != config.DefaultLimits() {
		t.Errorf("default: %+v", got)
	}
	c := st.Get()
	c.Limits.FPS = 500
	if err := st.Set(c); err == nil || !strings.Contains(err.Error(), "fps") {
		t.Errorf("fps 500: %v", err)
	}
}
