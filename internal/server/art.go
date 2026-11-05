package server

import (
	"bytes"
	"container/list"
	"crypto/sha1"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The DVR's pictures (guide images, channel logos) are large JPEGs and PNGs on
// https servers. Some TVs show none of them: an LG then makes its own
// previews, reading the recordings themselves, which is slow. The bridge
// hands the TVs its own address for each picture instead, /art/<key>.jpg,
// and serves it over http as a JPEG no larger than a DLNA thumbnail.

const (
	artSize      = 160     // JPEG_TN: at most 160x160
	maxArtKeys   = 20000   // pictures the bridge will answer for
	maxArtCached = 1000    // thumbnails kept made
	maxArtFetch  = 8 << 20 // the largest picture fetched
)

type artStore struct {
	mu   sync.Mutex
	urls map[string]string // key: the picture's address
	made map[string]*list.Element
	lru  *list.List // of *artEntry, most recently used first
}

type artEntry struct {
	key  string
	data []byte
}

// artURL is the bridge's address for a picture, as the client reaches the
// bridge. Only addresses handed out here are fetched: a client cannot have
// the bridge fetch whatever it names.
func (s *Server) artURL(v view, u string) string {
	if u == "" || !strings.HasPrefix(u, "http") || strings.HasPrefix(u, "http://"+v.host+"/") {
		return u
	}
	key := fmt.Sprintf("%x", sha1.Sum([]byte(u)))[:20]
	a := &s.art
	a.mu.Lock()
	if a.urls == nil || len(a.urls) >= maxArtKeys {
		a.urls = map[string]string{}
	}
	a.urls[key] = u
	a.mu.Unlock()
	return "http://" + v.host + "/art/" + key + ".jpg"
}

func (s *Server) serveArt(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSuffix(r.PathValue("name"), ".jpg")
	a := &s.art
	a.mu.Lock()
	u := a.urls[key]
	var data []byte
	if el := a.made[key]; el != nil {
		a.lru.MoveToFront(el)
		data = el.Value.(*artEntry).data
	}
	a.mu.Unlock()
	if u == "" {
		http.NotFound(w, r)
		return
	}
	if data == nil {
		var err error
		if data, err = s.thumbnail(r, u); err != nil {
			s.cfg.Logf("art: %s: %v", u, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.cfg.Logf("art: made %s for %s, %d bytes", key, s.who(r), len(data))
		a.mu.Lock()
		if a.made == nil {
			a.made, a.lru = map[string]*list.Element{}, list.New()
		}
		if a.made[key] == nil {
			a.made[key] = a.lru.PushFront(&artEntry{key, data})
			for a.lru.Len() > maxArtCached {
				delete(a.made, a.lru.Remove(a.lru.Back()).(*artEntry).key)
			}
		}
		a.mu.Unlock()
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Header().Set("ContentFeatures.DLNA.ORG", "DLNA.ORG_PN=JPEG_TN")
	w.Write(data)
}

// thumbnail fetches a picture and makes it a JPEG that fits in artSize.
func (s *Server) thumbnail(r *http.Request, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.agent())
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	src, _, err := image.Decode(io.LimitReader(resp.Body, maxArtFetch))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, fit(src, artSize), &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// fit scales an image down to fit in max x max, keeping its shape, by
// averaging the source pixels under each new one. Transparent parts go white.
func fit(src image.Image, limit int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	w, h := sw, sh
	if w > limit || h > limit {
		if w >= h {
			w, h = limit, max(1, sh*limit/sw)
		} else {
			w, h = max(1, sw*limit/sh), limit
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := sb.Min.Y+y*sh/h, sb.Min.Y+(y+1)*sh/h
		for x := 0; x < w; x++ {
			x0, x1 := sb.Min.X+x*sw/w, sb.Min.X+(x+1)*sw/w
			var r, g, bl, n uint64
			for sy := y0; sy < max(y1, y0+1); sy++ {
				for sx := x0; sx < max(x1, x0+1); sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					// Over white.
					r += uint64(cr + (0xffff - ca))
					g += uint64(cg + (0xffff - ca))
					bl += uint64(cb + (0xffff - ca))
					n++
				}
			}
			dst.Set(x, y, color.RGBA64{uint16(r / n), uint16(g / n), uint16(bl / n), 0xffff})
		}
	}
	return dst
}
