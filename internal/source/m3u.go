package source

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one channel of a playlist.
type Entry struct {
	// ID is its tvg-id, or for one without, the start of a hash of its URL.
	ID     string
	Number string // its tvg-chno, or its place after the numbered ones
	Name   string
	Logo   string
	Group  string // group-title
	URL    string
}

// Playlist is an M3U list of channels, read again every hour. The last good
// copy is kept in Cache, so that the channels are there after a restart while
// the list's host is down.
type Playlist struct {
	URL   string
	Cache string // empty: none is kept
	Agent string
	Logf  func(string, ...any)

	mu      sync.Mutex
	entries []Entry
	fetched time.Time
	err     error
}

// playlistEvery is how often the list is read again, and playlistRetry how
// soon after a failure while there is no copy at all.
const (
	playlistEvery = time.Hour
	playlistRetry = time.Minute
	maxPlaylist   = 32 << 20
)

// CacheFile is where the copy of the playlist at u is kept in dir; empty with
// no dir.
func CacheFile(dir, u string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "playlist-"+urlID(u)+".m3u")
}

// OpenPlaylist reads the playlist, or its kept copy when it cannot.
func OpenPlaylist(ctx context.Context, u, cache, agent string, logf func(string, ...any)) *Playlist {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	p := &Playlist{URL: u, Cache: cache, Agent: agent, Logf: logf}
	if cache != "" {
		if data, err := os.ReadFile(cache); err == nil {
			p.entries = Parse(data, u)
			if fi, err := os.Stat(cache); err == nil {
				p.fetched = fi.ModTime()
			}
		}
	}
	p.refresh(ctx)
	return p
}

// Entries are the channels, in number order.
func (p *Playlist) Entries() []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries
}

// Entry is one channel by its ID.
func (p *Playlist) Entry(id string) (Entry, bool) {
	for _, e := range p.Entries() {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Status is how the list was last read: its channels, when it was last read
// whole, and the error of the last try, if it failed.
func (p *Playlist) Status() (n int, fetched time.Time, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries), p.fetched, p.err
}

// Run reads the list again every hour, and calls changed when its channels
// changed.
func (p *Playlist) Run(ctx context.Context, changed func()) {
	for {
		wait := playlistEvery
		if n, _, err := p.Status(); n == 0 && err != nil {
			wait = playlistRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if p.refresh(ctx) && changed != nil {
			changed()
		}
	}
}

// refresh reads the list, and says whether its channels changed.
func (p *Playlist) refresh(ctx context.Context) bool {
	data, err := get(ctx, p.URL, p.Agent, 30*time.Second, maxPlaylist)
	if err == nil && !IsPlaylist(data) {
		err = errors.New("not an M3U playlist")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		if p.err == nil {
			p.Logf("playlist: %v; serving the %d channels read before", err, len(p.entries))
		}
		p.err = err
		return false
	}
	entries := Parse(data, p.URL)
	same := fmt.Sprint(entries) == fmt.Sprint(p.entries)
	p.entries, p.fetched, p.err = entries, time.Now(), nil
	if !same {
		p.Logf("playlist: %d channels", len(entries))
		if p.Cache != "" {
			tmp := p.Cache + ".tmp"
			if err := os.WriteFile(tmp, data, 0o644); err == nil {
				os.Rename(tmp, p.Cache)
			}
		}
	}
	return !same
}

var attrRE = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)

// Parse reads a playlist. base is its address, against which relative stream
// addresses are read. An HLS stream rather than a list of channels is one
// channel. Entries that are not http(s) streams are left out.
func Parse(data []byte, base string) []Entry {
	b, _ := url.Parse(base)
	if bytes.Contains(data, []byte("#EXT-X-TARGETDURATION")) || bytes.Contains(data, []byte("#EXT-X-STREAM-INF")) {
		name := strings.TrimSuffix(path.Base(b.Path), path.Ext(b.Path))
		if name == "" || name == "." || name == "/" {
			name = b.Hostname()
		}
		return []Entry{{ID: urlID(base), Number: "1", Name: name, URL: base}}
	}
	var out []Entry
	var cur *Entry
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			cur = &Entry{}
			info := line[len("#EXTINF:"):]
			for _, m := range attrRE.FindAllStringSubmatch(info, -1) {
				switch strings.ToLower(m[1]) {
				case "tvg-id":
					cur.ID = strings.TrimSpace(m[2])
				case "tvg-name":
					cur.Name = strings.TrimSpace(m[2])
				case "tvg-logo":
					cur.Logo = strings.TrimSpace(m[2])
				case "tvg-chno":
					cur.Number = strings.TrimSpace(m[2])
				case "group-title":
					cur.Group = strings.TrimSpace(m[2])
				}
			}
			// The title follows the first comma outside quotes.
			quoted := false
			for i, c := range info {
				if c == '"' {
					quoted = !quoted
				}
				if c == ',' && !quoted {
					if t := strings.TrimSpace(info[i+1:]); t != "" {
						cur.Name = t
					}
					break
				}
			}
		case strings.HasPrefix(line, "#EXTGRP:"):
			if cur != nil && cur.Group == "" {
				cur.Group = strings.TrimSpace(line[len("#EXTGRP:"):])
			}
		case line == "" || strings.HasPrefix(line, "#"):
		default:
			if cur == nil {
				continue // a stream with no #EXTINF
			}
			u, err := b.Parse(line)
			if err == nil && (u.Scheme == "http" || u.Scheme == "https") {
				cur.URL = u.String()
				out = append(out, *cur)
			}
			cur = nil
		}
	}
	return number(unique(out))
}

// unique gives each entry an ID of its own: its tvg-id, or the hash of its URL
// when it has none or another entry has it. The same stream twice is kept once.
func unique(in []Entry) []Entry {
	seen := map[string]bool{} // IDs and URLs
	out := in[:0]
	for _, e := range in {
		if seen[e.URL] {
			continue
		}
		if e.ID == "" || seen[e.ID] {
			e.ID = urlID(e.URL)
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID], seen[e.URL] = true, true
		if e.Name == "" {
			e.Name = e.ID
		}
		out = append(out, e)
	}
	return out
}

func urlID(u string) string { return fmt.Sprintf("%x", sha1.Sum([]byte(u)))[:8] }

// number orders the entries by tvg-chno, and numbers those without one after
// the highest, in the order of the list.
func number(es []Entry) []Entry {
	top := 0
	for _, e := range es {
		if n, err := strconv.ParseFloat(e.Number, 64); err == nil && int(n) > top {
			top = int(n)
		}
	}
	var numbered, rest []Entry
	for _, e := range es {
		if _, err := strconv.ParseFloat(e.Number, 64); err == nil {
			numbered = append(numbered, e)
		} else {
			rest = append(rest, e)
		}
	}
	sort.SliceStable(numbered, func(i, j int) bool {
		a, _ := strconv.ParseFloat(numbered[i].Number, 64)
		b, _ := strconv.ParseFloat(numbered[j].Number, 64)
		return a < b
	})
	for i := range rest {
		rest[i].Number = strconv.Itoa(top + i + 1)
	}
	return append(numbered, rest...)
}
