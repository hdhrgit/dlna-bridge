// Package config holds the settings edited in the web page and kept in a JSON
// file: how each kind of client is served.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Modes say where a client's streams come from.
const (
	ModeDirect    = "direct"    // the client plays the DVR's own stream
	ModeProxy     = "proxy"     // the same stream, passed through this server with DLNA headers (the default)
	ModeTranscode = "transcode" // converted by ffmpeg to the settings below
)

// Ads say what happens to commercial breaks the DVR has marked in a recording.
const (
	AdsCut  = "cut"  // left out
	AdsKeep = "keep" // played
)

// Seeks say how a client seeks in a recording with its breaks cut, and in a
// converted one.
const (
	// SeekBytes: a cut recording has a length and takes byte ranges, which
	// every TV knows; the cuts are all worked out before the first byte. A
	// converted recording has no length and does not seek.
	SeekBytes = "bytes"
	// SeekTime: a cut or converted recording has no length and takes a time to
	// start from (TimeSeekRange.dlna.org). A cut one starts at once, and each
	// cut is worked out just before playback reaches it.
	SeekTime = "time"
)

// Playbacks say whether where a client leaves a recording, and that it played
// one to the end, go to the DVR.
const (
	PlaybackShare = "share" // the DVR is told, so the Channels apps resume there too
	PlaybackKeep  = "keep"  // the DVR is not told
)

type Profile struct {
	Mode      string    `json:"mode"`
	Ads       string    `json:"ads"`
	Playback  string    `json:"playback"`
	Seek      string    `json:"seek"`
	Transcode Transcode `json:"transcode"`
}

// Transcode is what a client in transcode mode gets: the picture fitted inside
// MaxWidth x MaxHeight (or stretched to exactly that), an MPEG-TS stream.
type Transcode struct {
	MaxWidth      int    `json:"max_width"`
	MaxHeight     int    `json:"max_height"`
	FPS           int    `json:"fps"` // the most; a slower source keeps its rate
	VideoKbps     int    `json:"video_kbps"`
	AudioKbps     int    `json:"audio_kbps"`
	AudioChannels int    `json:"audio_channels"`
	H264Profile   string `json:"h264_profile"` // baseline, main or high
	// PreScale has the DVR make a small stream first for live channels, which
	// costs the DVR less than decoding the full picture here.
	PreScale bool `json:"pre_scale"`
	// ExactSize scales to exactly MaxWidth x MaxHeight, stretching a little if
	// the shapes differ, instead of fitting inside.
	ExactSize bool `json:"exact_size"`
	// Crop, a shape such as "4:3", keeps the middle of the picture in that
	// shape before scaling. Empty keeps the whole picture.
	Crop string `json:"crop,omitempty"`
	// SingleSlice makes each frame one slice, for decoders that take no more.
	SingleSlice bool `json:"single_slice"`
}

// CropShape reads Crop as width and height parts, false when it is empty or
// not a shape.
func (t Transcode) CropShape() (int, int, bool) {
	a, b, ok := strings.Cut(strings.TrimSpace(t.Crop), ":")
	if !ok {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(a)
	h, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || w < 1 || h < 1 || w > 100 || h > 100 {
		return 0, 0, false
	}
	return w, h, true
}

// Client is a group of devices that share a profile. A request matches when
// its address is in IPs or its User-Agent contains one of UserAgents. A client
// with neither is used only by name, with ?profile=<name>.
type Client struct {
	Name       string   `json:"name"`
	IPs        []string `json:"ips"` // addresses or CIDR ranges
	UserAgents []string `json:"user_agents"`
	Profile    Profile  `json:"profile"`
}

type Config struct {
	Default Profile  `json:"default"`
	Clients []Client `json:"clients"`
	// MaxTranscodes limits conversions running at once, across all clients.
	MaxTranscodes int `json:"max_transcodes"`
	// RecentHours is how far back the Recent recordings folder reaches.
	RecentHours int `json:"recent_hours"`
}

// DefaultTranscode is a start for a small screen.
func DefaultTranscode() Transcode {
	return Transcode{MaxWidth: 240, MaxHeight: 240, FPS: 24, VideoKbps: 250, AudioKbps: 64, AudioChannels: 1, H264Profile: "baseline", PreScale: true}
}

// Fill sets what is missing, so a short config file works.
func (c *Config) Fill() {
	fill := func(p *Profile) {
		if p.Mode == "" {
			p.Mode = ModeProxy // works for every TV; direct is an optimization to opt into
		}
		if p.Ads == "" {
			p.Ads = AdsCut
		}
		if p.Playback == "" {
			p.Playback = PlaybackShare
		}
		if p.Seek == "" {
			p.Seek = SeekBytes
		}
		if p.Transcode == (Transcode{}) {
			p.Transcode = DefaultTranscode()
		}
	}
	fill(&c.Default)
	for i := range c.Clients {
		fill(&c.Clients[i].Profile)
	}
	if c.MaxTranscodes == 0 {
		c.MaxTranscodes = 2
	}
	if c.RecentHours == 0 {
		c.RecentHours = 24
	}
}

func (p Profile) validate() error {
	switch p.Mode {
	case ModeDirect, ModeProxy, ModeTranscode:
	default:
		return fmt.Errorf("mode %q is not direct, proxy or transcode", p.Mode)
	}
	switch p.Ads {
	case AdsCut, AdsKeep:
	default:
		return fmt.Errorf("ads %q is not cut or keep", p.Ads)
	}
	switch p.Playback {
	case "", PlaybackShare, PlaybackKeep:
	default:
		return fmt.Errorf("playback %q is not share or keep", p.Playback)
	}
	switch p.Seek {
	case "", SeekBytes, SeekTime: // empty is bytes, from a file older than the setting
	default:
		return fmt.Errorf("seek %q is not bytes or time", p.Seek)
	}
	if p.Mode != ModeTranscode {
		return nil
	}
	t := p.Transcode
	switch {
	case t.MaxWidth < 16 || t.MaxWidth > 7680 || t.MaxHeight < 16 || t.MaxHeight > 4320:
		return errors.New("transcode size must be between 16 and 7680 x 4320")
	case t.FPS < 1 || t.FPS > 120:
		return errors.New("transcode fps must be 1 to 120")
	case t.VideoKbps < 16 || t.VideoKbps > 100000:
		return errors.New("transcode video kbps must be 16 to 100000")
	case t.AudioKbps < 8 || t.AudioKbps > 640:
		return errors.New("transcode audio kbps must be 8 to 640")
	case t.AudioChannels < 1 || t.AudioChannels > 6:
		return errors.New("transcode audio channels must be 1 to 6")
	}
	if _, _, ok := t.CropShape(); strings.TrimSpace(t.Crop) != "" && !ok {
		return fmt.Errorf("crop %q is not a shape such as 4:3", t.Crop)
	}
	switch t.H264Profile {
	case "baseline", "main", "high":
	default:
		return fmt.Errorf("h264 profile %q is not baseline, main or high", t.H264Profile)
	}
	return nil
}

func (c *Config) Validate() error {
	if err := c.Default.validate(); err != nil {
		return fmt.Errorf("default: %w", err)
	}
	if c.MaxTranscodes < 1 || c.MaxTranscodes > 64 {
		return errors.New("max transcodes must be 1 to 64")
	}
	if c.RecentHours < 1 || c.RecentHours > 24*365 {
		return errors.New("recent hours must be 1 to 8760")
	}
	names := map[string]bool{}
	for i, cl := range c.Clients {
		name := strings.TrimSpace(cl.Name)
		if name == "" {
			return fmt.Errorf("client %d has no name", i+1)
		}
		if names[strings.ToLower(name)] {
			return fmt.Errorf("two clients are named %q", name)
		}
		names[strings.ToLower(name)] = true
		for _, s := range cl.IPs {
			if _, err := parseNet(s); err != nil {
				return fmt.Errorf("client %q: %w", name, err)
			}
		}
		for _, s := range cl.UserAgents {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("client %q has an empty User-Agent text", name)
			}
		}
		if err := cl.Profile.validate(); err != nil {
			return fmt.Errorf("client %q: %w", name, err)
		}
	}
	return nil
}

// parseNet reads an address or a CIDR range.
func parseNet(s string) (*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an address or range", s)
		}
		return n, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("%q is not an address or range", s)
	}
	bits := 8 * len(ip.To16())
	if ip4 := ip.To4(); ip4 != nil {
		ip, bits = ip4, 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

// Match finds the profile for a request: the client called forced, if there
// is one, else the first whose address or User-Agent fits, else the default.
func (c *Config) Match(ip net.IP, ua, forced string) (string, Profile) {
	if forced != "" {
		for _, cl := range c.Clients {
			if strings.EqualFold(cl.Name, forced) {
				return cl.Name, cl.Profile
			}
		}
	}
	lua := strings.ToLower(ua)
	for _, cl := range c.Clients {
		for _, s := range cl.IPs {
			if n, err := parseNet(s); err == nil && ip != nil && n.Contains(ip) {
				return cl.Name, cl.Profile
			}
		}
		for _, s := range cl.UserAgents {
			if s = strings.ToLower(strings.TrimSpace(s)); s != "" && strings.Contains(lua, s) {
				return cl.Name, cl.Profile
			}
		}
	}
	return "", c.Default
}

// Store keeps the config in memory and in its file.
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// Open reads the file. If there is none, start is used until the first save.
func Open(path string, start Config) (*Store, error) {
	s := &Store{path: path, cfg: start}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || path == "" {
		s.cfg.Fill()
		return s, s.cfg.Validate()
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Fill()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.cfg = c
	return s, nil
}

// Get returns a copy of the config.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

func clone(c Config) Config {
	out := c
	out.Clients = make([]Client, len(c.Clients))
	for i, cl := range c.Clients {
		cl.IPs = append([]string(nil), cl.IPs...)
		cl.UserAgents = append([]string(nil), cl.UserAgents...)
		out.Clients[i] = cl
	}
	return out
}

// Set checks the config, writes it to the file, and uses it from now on.
func (s *Store) Set(c Config) error {
	c = clone(c)
	c.Fill()
	for i := range c.Clients {
		c.Clients[i].Name = strings.TrimSpace(c.Clients[i].Name)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if s.path != "" {
		data, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
			return err
		}
		tmp := s.path + ".tmp"
		if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, s.path); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()
	return nil
}

// Match is Config.Match on the current config.
func (s *Store) Match(ip net.IP, ua, forced string) (string, Profile) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Match(ip, ua, forced)
}

// Persistent says whether Set keeps the config in a file.
func (s *Store) Persistent() bool { return s.path != "" }

func (s *Store) RecentHours() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.RecentHours
}

func (s *Store) MaxTranscodes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.MaxTranscodes
}
