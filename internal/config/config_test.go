package config

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	c := Config{
		Default: Profile{Mode: ModeDirect, Ads: AdsCut},
		Clients: []Client{
			{Name: "Tiny", IPs: []string{"10.0.0.7", "192.168.5.0/24"}, Profile: Profile{Mode: ModeTranscode}},
			{Name: "Samsung", UserAgents: []string{"samsung"}, Profile: Profile{Mode: ModeProxy}},
			{Name: "Board", Profile: Profile{Mode: ModeTranscode}}, // by name only
		},
	}
	for _, tc := range []struct{ ip, ua, forced, want string }{
		{"10.0.0.7", "", "", "Tiny"},
		{"192.168.5.99", "Samsung", "", "Tiny"}, // the first client that fits wins
		{"10.0.0.8", "SEC_HHP_[TV] Samsung 7", "", "Samsung"},
		{"10.0.0.8", "LG webOS", "", ""},
		{"10.0.0.8", "LG webOS", "samsung", "Samsung"}, // forced by name, any case
		{"10.0.0.8", "LG webOS", "nobody", ""},
		{"10.0.0.8", "LG webOS", "board", "Board"},
	} {
		if got, _ := c.Match(net.ParseIP(tc.ip), tc.ua, tc.forced); got != tc.want {
			t.Errorf("%+v: matched %q", tc, got)
		}
	}
}

func TestValidate(t *testing.T) {
	good := Config{Default: Profile{Mode: ModeDirect, Ads: AdsCut}, MaxTranscodes: 2, RecentHours: 24, Limits: DefaultLimits(),
		Clients: []Client{{Name: "A", IPs: []string{"10.0.0.1"}, Profile: Profile{Mode: ModeTranscode, Ads: AdsKeep, Transcode: DefaultTranscode()}}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	named := clone(good)
	named.Clients[0].IPs = nil // used only by ?profile=
	named.Clients[0].Profile.Transcode.Crop = "4:3"
	if err := named.Validate(); err != nil {
		t.Errorf("name-only client with a crop: %v", err)
	}
	for name, edit := range map[string]func(*Config){
		"mode":        func(c *Config) { c.Default.Mode = "fast" },
		"crop":        func(c *Config) { c.Clients[0].Profile.Transcode.Crop = "wide" },
		"bad address": func(c *Config) { c.Clients[0].IPs = []string{"10.0.0"} },
		"duplicate": func(c *Config) {
			c.Clients = append(c.Clients, Client{Name: "a", IPs: []string{"1.1.1.1"}, Profile: c.Default})
		},
		"fps":           func(c *Config) { c.Clients[0].Profile.Transcode.FPS = 0 },
		"h264":          func(c *Config) { c.Clients[0].Profile.Transcode.H264Profile = "extended" },
		"max transcode": func(c *Config) { c.MaxTranscodes = 0 },
		"recent hours":  func(c *Config) { c.RecentHours = 0 },
		"kind":          func(c *Config) { c.Clients[0].Profile.Kind = "radio" },
		"audio kbps":    func(c *Config) { c.Clients[0].Profile.Audio.Kbps = 1000 },
		"limit width":   func(c *Config) { c.Limits.MaxWidth = 8 },
	} {
		c := clone(good)
		edit(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	s, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Get(); got.Default.Mode != ModeProxy || got.MaxTranscodes != 2 || got.RecentHours != 24 {
		t.Errorf("defaults: %+v", got)
	}
	c := s.Get()
	c.Clients = []Client{{Name: " Bedroom ", UserAgents: []string{"lg"}, Profile: Profile{Mode: ModeProxy}}}
	if err := s.Set(c); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); len(got.Clients) != 1 || got.Clients[0].Name != "Bedroom" || got.Clients[0].Profile.Ads != AdsCut {
		t.Errorf("after reload: %+v", got)
	}
	c.Clients[0].Profile.Mode = "fast"
	if err := s.Set(c); err == nil || !strings.Contains(err.Error(), "fast") {
		t.Errorf("bad config saved: %v", err)
	}
	if got := s.Get(); got.Clients[0].Profile.Mode != ModeProxy {
		t.Error("a refused config replaced the good one")
	}
}
