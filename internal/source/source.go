// Package source finds out what the bridge was given to serve in place of the
// DVR from -dvr, and reads M3U playlists.
package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The kinds of source.
const (
	Auto     = "auto"
	Channels = "channels"
	DLNA     = "dlna"
	M3U      = "m3u"
)

// IsURL says whether addr is an http(s) URL rather than a host.
func IsURL(addr string) bool {
	a := strings.ToLower(addr)
	return strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://")
}

// DVRAddress is the address of a Channels DVR given as a host, with or
// without a port, or as a URL: the URL as it is, else the host on port 8089.
func DVRAddress(addr string) string {
	if IsURL(addr) {
		return strings.TrimRight(addr, "/")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), "8089")
	}
	return "http://" + addr
}

// Detect finds out what addr is: an M3U playlist, a Channels DVR, or (below)
// a DLNA server. where is the address to use for that kind.
func Detect(ctx context.Context, addr, agent string) (kind, where string, err error) {
	if IsURL(addr) {
		body, err := get(ctx, addr, agent, 10*time.Second, 64<<10)
		if err != nil {
			return "", "", err
		}
		if IsPlaylist(body) {
			return M3U, addr, nil
		}
		if IsDVR(ctx, DVRAddress(addr), agent) {
			return Channels, DVRAddress(addr), nil
		}
		return "", "", fmt.Errorf("%s is neither an M3U playlist nor a Channels DVR", addr)
	}
	if base := DVRAddress(addr); IsDVR(ctx, base, agent) {
		return Channels, base, nil
	}
	return "", "", fmt.Errorf("no Channels DVR answers at %s", DVRAddress(addr))
}

// IsDVR says whether a Channels DVR answers at base: its /dvr gives the DVR's
// status, within 3 seconds.
func IsDVR(ctx context.Context, base, agent string) bool {
	body, err := get(ctx, base+"/dvr", agent, 3*time.Second, 1<<20)
	if err != nil {
		return false
	}
	var d map[string]json.RawMessage
	if json.Unmarshal(body, &d) != nil {
		return false
	}
	_, activity := d["activity"]
	_, disk := d["disk"]
	return activity && disk
}

// IsPlaylist says whether a body is an M3U playlist: it starts #EXTM3U.
func IsPlaylist(body []byte) bool {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("#EXTM3U"))
}

// get fetches the start of u, at most limit bytes, in at most timeout.
func get(ctx context.Context, u, agent string, timeout time.Duration, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if agent != "" {
		req.Header.Set("User-Agent", agent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}
