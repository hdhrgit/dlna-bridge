package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"dlna/internal/config"
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
	head := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-user_agent", "UA", "-live_start_index", "-1", "-i", "IN", "-map", "0:v:0", "-map", "0:a:0?", "-vf"}
	tail := []string{"-c:v", "libx264", "-profile:v", "baseline", "-preset", "veryfast", "-tune", "zerolatency", "-x264-params", "sliced-threads=0:slices=1", "-g", "48", "-b:v", "250k", "-maxrate", "300k", "-bufsize", "500k", "-a53cc", "0", "-c:a", "aac", "-ac", "1", "-ar", "44100", "-b:a", "64k", "-flush_packets", "1", "-f", "mpegts", "-muxdelay", "0", "-muxpreload", "0", "pipe:1"}
	want := map[string]string{
		"esp32":     `fps=min(source_fps\,24),scale=240:144,setsar=1,format=yuv420p`,
		"esp32_4x3": `fps=min(source_fps\,24),crop=min(iw\,ih*4/3):min(ih\,iw*3/4),scale=240:192,setsar=1,format=yuv420p`,
	}
	settings := mustSettings(t, config.Config{Clients: boardClients()})
	s := New(Config{Settings: settings})
	for name, vf := range want {
		_, prof := s.profileFor(httptest.NewRequest("GET", "/stream/live/5?profile="+name, nil))
		got := ffmpegArgs(prof.Transcode, "IN", true, "UA", 0)
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
		{"w=321&h=181", "320x180 fit=scale fps=24 vb=250 ab=64"},                            // down to even
		{"w=9999&h=9999&fps=60&vb=99999&ab=999", "640x480 fit=scale fps=30 vb=1500 ab=128"}, // clamped to the limits
		{"w=3&h=1&fps=1&vb=1&ab=1", "16x16 fit=scale fps=1 vb=16 ab=8"},                     // and to the least
		{"w=abc&h=-5&fps=0&vb=1.5&fit=zoom", "240x144 fit=scale fps=24 vb=250 ab=64"},       // not values: ignored
		{"w=448&h=252&fit=crop43", "448x252 fit=crop43 fps=24 vb=250 ab=64"},
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
	// Codec, profile and slices stay the base profile's.
	got = withOverrides(base, url.Values{"w": {"320"}, "h": {"180"}}, lim)
	if got.H264Profile != base.H264Profile || got.SingleSlice != base.SingleSlice || got.PreScale != base.PreScale || got.AudioChannels != base.AudioChannels {
		t.Errorf("base settings changed: %+v", got)
	}
	// Smaller limits from the config.
	small := config.Limits{MaxWidth: 321, MaxHeight: 200, FPS: 20, VideoKbps: 300, AudioKbps: 48}
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
	if got := get(newTestServer(t, true)); got != `{"max":{"ab":128,"fps":30,"h":480,"vb":1500,"w":640},"overrides":["w","h","fit","fps","vb","ab"]}` {
		t.Errorf("default: %s", got)
	}
	s := newServerWith(t, config.Config{Limits: config.Limits{MaxWidth: 448, MaxHeight: 252, FPS: 25, VideoKbps: 800, AudioKbps: 96}})
	if got := get(s); got != `{"max":{"ab":96,"fps":25,"h":252,"vb":800,"w":448},"overrides":["w","h","fit","fps","vb","ab"]}` {
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
}

// Limits missing from a config file are the defaults; ones out of range are
// refused like any other setting.
func TestLimitsInConfig(t *testing.T) {
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
