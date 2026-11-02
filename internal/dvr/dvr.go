// Package dvr reads channels, the guide and recordings from a Channels DVR.
package dvr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Channel is one live channel, from /api/v1/channels.
type Channel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Number string `json:"number"`
	Logo   string `json:"logo_url"`
	HD     bool   `json:"hd"`
	Source string `json:"source_name"`
	// Favorited and Hidden are the flags set in the Channels apps.
	Favorited bool `json:"favorited"`
	Hidden    bool `json:"hidden"`
}

// Airing is one show in the guide or one recorded show.
type Airing struct {
	Title        string     `json:"Title"`
	EpisodeTitle string     `json:"EpisodeTitle"`
	Summary      string     `json:"Summary"`
	Image        string     `json:"Image"`
	Channel      flexString `json:"Channel"`
	Time         int64      `json:"Time"`     // Unix seconds
	Duration     float64    `json:"Duration"` // seconds
	Season       int        `json:"SeasonNumber"`
	Episode      int        `json:"EpisodeNumber"`
	OriginalDate string     `json:"OriginalDate"`
}

// Recording is one file in /dvr/files.
type Recording struct {
	ID           flexString `json:"ID"`
	GroupID      flexString `json:"GroupID"`
	Airing       Airing     `json:"Airing"`
	Thumbnail    string     `json:"Thumbnail"`
	Duration     float64    `json:"Duration"`     // seconds
	CreatedAt    int64      `json:"CreatedAt"`    // Unix seconds; see Created
	PlaybackTime float64    `json:"PlaybackTime"` // seconds watched in the Channels apps
	FileSize     int64      `json:"FileSize"`
	// Commercials are the breaks the DVR found: start, end, start, end... in
	// seconds. Empty until the recording is processed.
	Commercials []float64 `json:"Commercials"`
	Completed   bool      `json:"Completed"`
	Processed   bool      `json:"Processed"`
	Deleted     bool      `json:"Deleted"`
}

// AdFree is the time in the recording with the breaks taken out. t of -1 is
// the end.
func (r Recording) AdFree(t float64) float64 {
	if t < 0 {
		t = r.Duration
	}
	out := t
	for i := 0; i+1 < len(r.Commercials); i += 2 {
		from, to := r.Commercials[i], min(r.Commercials[i+1], t)
		if to > from {
			out -= to - from
		}
	}
	return max(out, 0)
}

// Created is when the recording started. The DVR gives seconds; older
// versions gave milliseconds.
func (r Recording) Created() time.Time {
	if r.CreatedAt > 1e11 {
		return time.UnixMilli(r.CreatedAt)
	}
	return time.Unix(r.CreatedAt, 0)
}

// Group is a show folder in /dvr/groups.
type Group struct {
	ID    flexString `json:"ID"`
	Name  string     `json:"Name"`
	Image string     `json:"Image"`
}

// flexString takes a JSON string or number, since ids are strings in some
// DVR versions and numbers in others.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	*f = flexString(b)
	return nil
}

// Client talks to one DVR. Lists are kept for a short while, because a TV
// browses the same folder several times in a row.
type Client struct {
	Base *url.URL
	HTTP *http.Client

	channels cached[[]Channel]
	now      cached[[]nowEntry]
	files    cached[[]Recording]
	groups   cached[[]Group]
}

func New(base string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("DVR address %q is not an http:// address", base)
	}
	return &Client{Base: u, HTTP: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base.String()+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("DVR %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("DVR %s: %w", path, err)
	}
	return nil
}

// Channels lists the live channels in number order, one per number.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	return c.channels.get(time.Minute, func() ([]Channel, error) {
		var all []Channel
		if err := c.getJSON(ctx, "/api/v1/channels", &all); err != nil {
			return nil, err
		}
		// Several sources can carry a number; the DVR picks one when it plays
		// it, so list the number once, as a favorite if any of them is.
		at := map[string]int{}
		out := all[:0]
		for _, ch := range all {
			if ch.Number == "" || ch.Hidden {
				continue
			}
			if i, ok := at[ch.Number]; ok {
				out[i].Favorited = out[i].Favorited || ch.Favorited
				continue
			}
			at[ch.Number] = len(out)
			out = append(out, ch)
		}
		sort.SliceStable(out, func(i, j int) bool { return numberLess(out[i].Number, out[j].Number) })
		return out, nil
	})
}

// Channel finds a channel by its number.
func (c *Client) Channel(ctx context.Context, number string) (Channel, bool, error) {
	chs, err := c.Channels(ctx)
	if err != nil {
		return Channel{}, false, err
	}
	for _, ch := range chs {
		if ch.Number == number {
			return ch, true, nil
		}
	}
	return Channel{}, false, nil
}

type nowEntry struct {
	Airings []Airing `json:"Airings"`
	Channel struct {
		Number string `json:"Number"`
	} `json:"Channel"`
}

// NowPlaying maps a channel number to the title of the show on now.
func (c *Client) NowPlaying(ctx context.Context) (map[string]string, error) {
	entries, err := c.now.get(time.Minute, func() ([]nowEntry, error) {
		var e []nowEntry
		err := c.getJSON(ctx, "/devices/ANY/guide/now", &e)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	return nowTitles(entries, time.Now()), nil
}

func nowTitles(entries []nowEntry, at time.Time) map[string]string {
	t := at.Unix()
	out := map[string]string{}
	for _, e := range entries {
		for _, a := range e.Airings {
			if a.Time <= t && float64(t) < float64(a.Time)+a.Duration {
				title := a.Title
				if a.EpisodeTitle != "" {
					title += ": " + a.EpisodeTitle
				}
				out[e.Channel.Number] = title
				break
			}
		}
	}
	return out
}

// Recordings lists the recordings that are not deleted, newest first.
func (c *Client) Recordings(ctx context.Context) ([]Recording, error) {
	return c.files.get(30*time.Second, func() ([]Recording, error) {
		var all []Recording
		if err := c.getJSON(ctx, "/dvr/files", &all); err != nil {
			return nil, err
		}
		out := all[:0]
		for _, r := range all {
			if !r.Deleted && r.ID != "" {
				out = append(out, r)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
		return out, nil
	})
}

// Groups lists the show folders.
func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	return c.groups.get(30*time.Second, func() ([]Group, error) {
		var g []Group
		err := c.getJSON(ctx, "/dvr/groups", &g)
		return g, err
	})
}

// LivePath is the DVR's own MPEG-TS stream of a channel, at full quality.
func LivePath(number string) string {
	return "/devices/ANY/channels/" + url.PathEscape(number) + "/stream.mpg"
}

// RecordingPath is the DVR's stream of a recorded file. It takes byte ranges.
func RecordingPath(id string) string {
	return "/dvr/files/" + url.PathEscape(id) + "/stream.mpg"
}

// Recording finds a recording by id.
func (c *Client) Recording(ctx context.Context, id string) (Recording, bool, error) {
	all, err := c.Recordings(ctx)
	if err != nil {
		return Recording{}, false, err
	}
	for _, r := range all {
		if string(r.ID) == id {
			return r, true, nil
		}
	}
	return Recording{}, false, nil
}

// File reads byte ranges of one recording.
type File struct {
	c    *Client
	path string
	size int64
}

func (c *Client) File(r Recording) *File {
	return &File{c: c, path: RecordingPath(string(r.ID)), size: r.FileSize}
}

func (f *File) Size() int64 { return f.size }

func (f *File) ReadAt(ctx context.Context, off, n int64) ([]byte, error) {
	body, err := f.Open(ctx, off, n)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// Open streams n bytes from off.
func (f *File) Open(ctx context.Context, off, n int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", f.c.Base.String()+f.path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	// No client timeout: the body may be a long stretch of a recording.
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("DVR %s bytes %d-%d: %s", f.path, off, off+n-1, resp.Status)
	}
	return resp.Body, nil
}

// SmallStreamURL asks the DVR's transcoder for a small H.264/AAC HLS stream of
// a channel. height must be even, and bitrate is the total in kbps.
func (c *Client) SmallStreamURL(number string, height, kbps int) string {
	q := url.Values{}
	q.Set("vcodec", "h264")
	q.Set("acodec", "aac")
	q.Set("resolution", strconv.Itoa(height))
	q.Set("bitrate", strconv.Itoa(kbps))
	q.Set("abitrate", "64")
	return c.Base.String() + "/devices/ANY/channels/" + url.PathEscape(number) + "/hls/master.m3u8?" + q.Encode()
}

// numberLess orders channel numbers such as 5, 5.1 and 12 by value.
func numberLess(a, b string) bool {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil && fa != fb {
		return fa < fb
	}
	return a < b
}

// cached keeps the last good value for ttl. When a refresh fails it serves the
// stale value rather than an empty list.
type cached[T any] struct {
	mu sync.Mutex
	at time.Time
	ok bool
	v  T
}

func (c *cached[T]) get(ttl time.Duration, fetch func() (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && time.Since(c.at) < ttl {
		return c.v, nil
	}
	v, err := fetch()
	if err != nil {
		if c.ok {
			return c.v, nil
		}
		return v, err
	}
	c.v, c.at, c.ok = v, time.Now(), true
	return v, nil
}
