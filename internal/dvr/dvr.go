// Package dvr reads channels, the guide and recordings from a Channels DVR.
package dvr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	ID       string `json:"id"`
	Name     string `json:"name"`
	Number   string `json:"number"`
	Logo     string `json:"logo_url"`
	HD       bool   `json:"hd"`
	Source   string `json:"source_name"`
	SourceID string `json:"source_id"`
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
	Channel      FlexString `json:"Channel"`
	Time         int64      `json:"Time"`     // Unix seconds
	Duration     float64    `json:"Duration"` // seconds
	Season       int        `json:"SeasonNumber"`
	Episode      int        `json:"EpisodeNumber"`
	OriginalDate string     `json:"OriginalDate"`
}

// Recording is one file in /dvr/files.
type Recording struct {
	ID           FlexString `json:"ID"`
	GroupID      FlexString `json:"GroupID"`
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
	ID    FlexString `json:"ID"`
	Name  string     `json:"Name"`
	Image string     `json:"Image"`
}

// FlexString takes a JSON string or number, since ids are strings in some
// DVR versions and numbers in others.
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = FlexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	*f = FlexString(b)
	return nil
}

// Client talks to one DVR. Lists are kept for a short while, because a TV
// browses the same folder several times in a row.
type Client struct {
	Base *url.URL
	HTTP *http.Client
	// MaxStale is how old a list may be when the DVR does not answer and it is
	// served anyway. Past it the DVR's error is returned.
	MaxStale time.Duration
	// UserAgent names this program to the DVR, in place of Go's own
	// "Go-http-client/1.1".
	UserAgent string

	channels cached[[]Channel]
	now      cached[[]nowEntry]
	files    cached[[]Recording]
	groups   cached[[]Group]
	activity cached[activityResult]
}

type activityResult struct {
	Items []ActivityItem
	Took  time.Duration
}

// ActivityItem is one thing the DVR says it is doing, and the address of the
// client it does it for, when it names one ("6-file-57285-10.0.0.7").
type ActivityItem struct {
	Text string `json:"text"`
	IP   string `json:"ip,omitempty"`
}

// Activity lists what the DVR says it is doing: the streams being watched
// (direct ones too), the recordings under way, and the jobs it runs. Took is how
// long the DVR took to answer.
func (c *Client) Activity(ctx context.Context) ([]ActivityItem, time.Duration, error) {
	r, err := c.activity.get(ctx, 3*time.Second, 10*time.Second, func(ctx context.Context) (activityResult, error) {
		var d struct {
			Activity map[string]string `json:"activity"`
		}
		start := time.Now()
		if err := c.getJSON(ctx, "/dvr", &d); err != nil {
			return activityResult{}, err
		}
		took := time.Since(start)
		items := make([]ActivityItem, 0, len(d.Activity))
		for k, v := range d.Activity {
			it := ActivityItem{Text: v}
			if i := strings.LastIndexByte(k, '-'); i >= 0 && net.ParseIP(k[i+1:]) != nil {
				it.IP = k[i+1:]
			}
			items = append(items, it)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Text < items[j].Text })
		return activityResult{items, took}, nil
	})
	return r.Items, r.Took, err
}

func New(base string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("DVR address %q is not an http:// address", base)
	}
	return &Client{Base: u, HTTP: &http.Client{Timeout: 15 * time.Second}, MaxStale: 15 * time.Minute, UserAgent: "dlna-bridge"}, nil
}

// SetAgent marks a request to the DVR as this program's.
func (c *Client) SetAgent(req *http.Request) {
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base.String()+path, nil)
	if err != nil {
		return err
	}
	c.SetAgent(req)
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

// AllChannels lists the channels that are not hidden, in number order, a
// number once for each source that carries it.
func (c *Client) AllChannels(ctx context.Context) ([]Channel, error) {
	return c.channels.get(ctx, time.Minute, c.MaxStale, func(ctx context.Context) ([]Channel, error) {
		var all []Channel
		if err := c.getJSON(ctx, "/api/v1/channels", &all); err != nil {
			return nil, err
		}
		out := all[:0]
		for _, ch := range all {
			if ch.Number != "" && !ch.Hidden {
				out = append(out, ch)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return numberLess(out[i].Number, out[j].Number) })
		return out, nil
	})
}

// Channels lists the channels once for each number. Several sources can carry
// a number; the DVR picks one when it plays it, so it counts as a favorite if
// any of them is.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	all, err := c.AllChannels(ctx)
	if err != nil {
		return nil, err
	}
	at := map[string]int{}
	out := make([]Channel, 0, len(all))
	for _, ch := range all {
		if i, ok := at[ch.Number]; ok {
			out[i].Favorited = out[i].Favorited || ch.Favorited
			continue
		}
		at[ch.Number] = len(out)
		out = append(out, ch)
	}
	return out, nil
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
	entries, err := c.now.get(ctx, time.Minute, c.MaxStale, func(ctx context.Context) ([]nowEntry, error) {
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
	return c.files.get(ctx, 30*time.Second, c.MaxStale, func(ctx context.Context) ([]Recording, error) {
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
	return c.groups.get(ctx, 30*time.Second, c.MaxStale, func(ctx context.Context) ([]Group, error) {
		var g []Group
		err := c.getJSON(ctx, "/dvr/groups", &g)
		return g, err
	})
}

// LivePath is the DVR's own MPEG-TS stream of a channel, at full quality. device
// is the source to take it from, or empty for whichever the DVR picks.
func LivePath(device, number string) string {
	if device == "" {
		device = "ANY"
	}
	return "/devices/" + url.PathEscape(device) + "/channels/" + url.PathEscape(number) + "/stream.mpg"
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

// ForgetRecordings drops the cached recording and show lists, so that the next
// ask sees a change the DVR has announced.
func (c *Client) ForgetRecordings() {
	c.files.forget()
	c.groups.forget()
}

// ForgetGuide drops the cached shows on now, so that the next ask fetches them.
func (c *Client) ForgetGuide() { c.now.forget() }

// Event is one line of the DVR's event stream: files.created, files.updated,
// files.deleted, groups.updated, activities.set, hello...
type Event struct {
	Type    string `json:"Type"`
	Name    string `json:"Name"`
	Value   string `json:"Value"`
	Version string `json:"Version"` // in hello
}

// Events follows the DVR's event stream (/dvr/events/subscribe, one JSON
// object a line) and hands each event to fn, until the stream or ctx ends.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Base.String()+"/dvr/events/subscribe", nil)
	if err != nil {
		return err
	}
	c.SetAgent(req)
	// No client timeout: the stream stays open as long as the DVR runs.
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DVR /dvr/events/subscribe: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type != "" {
			fn(e)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}

// Newest is the DVR's latest additions, newest first: a short list, cheap to
// ask for often, unlike /dvr/files, which holds the whole library.
type Newest struct {
	ID          string    `json:"id"`
	Completed   bool      `json:"completed"`
	Processed   bool      `json:"processed"`
	Commercials []float64 `json:"commercials"`
}

func (c *Client) Newest(ctx context.Context, n int) ([]Newest, error) {
	var out []Newest
	err := c.getJSON(ctx, fmt.Sprintf("/api/v1/all?sort=date_added&order=desc&limit=%d", n), &out)
	return out, err
}

// RecordingByID asks the DVR for one recording, not from the cached list.
func (c *Client) RecordingByID(ctx context.Context, id string) (Recording, error) {
	var r Recording
	err := c.getJSON(ctx, "/dvr/files/"+url.PathEscape(id), &r)
	return r, err
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
	f.c.SetAgent(req)
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

// Register tells the DVR that this program is a client, called name. The DVR
// keeps one client for each address it is reached from, and names what that
// address watches, in its activity list and its client list, after it: without
// this the bridge's streams are named by its address. machineID is a stable
// identity for it. It returns the id the DVR gave the client.
func (c *Client) Register(ctx context.Context, name, machineID string) (string, error) {
	body, err := json.Marshal(map[string]string{"hostname": name, "machine_id": machineID})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.Base.String()+"/dvr/clients/register", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.SetAgent(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DVR /dvr/clients/register: %s", resp.Status)
	}
	var out struct {
		CID string `json:"cid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("DVR /dvr/clients/register: %w", err)
	}
	return out.CID, nil
}

// ClientEntry is one client in the DVR's client list.
type ClientEntry struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	LocalIP  string `json:"local_ip"`
}

// ClientInfo looks a client up in the DVR's client list by its id.
func (c *Client) ClientInfo(ctx context.Context, id string) (ClientEntry, bool, error) {
	var all []ClientEntry
	if err := c.getJSON(ctx, "/dvr/clients/info", &all); err != nil {
		return ClientEntry{}, false, err
	}
	for _, e := range all {
		if e.ID == id {
			return e, true, nil
		}
	}
	return ClientEntry{}, false, nil
}

// SmallStreamURL asks the DVR's transcoder for a small H.264/AAC HLS stream of
// a channel. height must be even, and bitrate is the total in kbps.
func (c *Client) SmallStreamURL(device, number string, height, kbps int) string {
	if device == "" {
		device = "ANY"
	}
	q := url.Values{}
	q.Set("vcodec", "h264")
	q.Set("acodec", "aac")
	q.Set("resolution", strconv.Itoa(height))
	q.Set("bitrate", strconv.Itoa(kbps))
	q.Set("abitrate", "64")
	return c.Base.String() + "/devices/" + url.PathEscape(device) + "/channels/" + url.PathEscape(number) + "/hls/master.m3u8?" + q.Encode()
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

// cached keeps a value fresh for ttl. After that the next request starts one
// refresh, and no more than one at a time however many ask. While it runs, a
// request is served the old value, unless that is older than maxStale: then it
// waits for the refresh and gets its error if it fails, rather than data that
// may long have stopped being true. The lock is never held while fetching.
type cached[T any] struct {
	mu   sync.Mutex
	v    T
	at   time.Time // when v was fetched
	ok   bool
	call *refresh[T]

	err   error // of the last refresh that failed, until one succeeds
	errAt time.Time
}

type refresh[T any] struct {
	done chan struct{}
	v    T
	err  error
}

// refreshTimeout limits a refresh, which runs apart from the request that
// started it.
const refreshTimeout = 20 * time.Second

func (c *cached[T]) get(ctx context.Context, ttl, maxStale time.Duration, fetch func(context.Context) (T, error)) (T, error) {
	c.mu.Lock()
	age := time.Since(c.at)
	if c.ok && age < ttl {
		v := c.v
		c.mu.Unlock()
		return v, nil
	}
	call := c.call
	if call == nil {
		call = &refresh[T]{done: make(chan struct{})}
		c.call = call
		go func() {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
			defer cancel()
			v, err := fetch(rctx)
			c.mu.Lock()
			if err == nil {
				c.v, c.at, c.ok, c.err = v, time.Now(), true, nil
			} else {
				c.err, c.errAt = err, time.Now()
			}
			c.call = nil
			c.mu.Unlock()
			call.v, call.err = v, err
			close(call.done)
		}()
	}
	if c.ok && age < maxStale {
		v := c.v
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	select {
	case <-call.done:
		return call.v, call.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// forget makes the next get fetch afresh and wait for it, rather than answer
// with what it has.
func (c *cached[T]) forget() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// health says when the value was last fetched and why the last refresh failed.
func (c *cached[T]) health() (at time.Time, err error, errAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at, c.err, c.errAt
}

// Health is how fresh one kind of DVR data is.
type Health struct {
	What      string    `json:"what"`
	LastOK    time.Time `json:"last_ok"` // zero: never fetched
	LastError string    `json:"last_error,omitempty"`
	ErrorAt   time.Time `json:"error_at,omitempty"`
}

// Health reports on each list the client keeps. A DVR that stops answering
// shows here before the lists go stale enough to be dropped.
func (c *Client) Health() []Health {
	out := []Health{}
	add := func(what string, at time.Time, err error, errAt time.Time) {
		h := Health{What: what, LastOK: at}
		if err != nil {
			h.LastError, h.ErrorAt = err.Error(), errAt
		}
		out = append(out, h)
	}
	at, err, errAt := c.channels.health()
	add("channels", at, err, errAt)
	at, err, errAt = c.now.health()
	add("guide", at, err, errAt)
	at, err, errAt = c.files.health()
	add("recordings", at, err, errAt)
	at, err, errAt = c.groups.health()
	add("shows", at, err, errAt)
	return out
}
