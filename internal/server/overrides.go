package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"dlna/internal/config"
)

// overrideParams are what a stream request may change in its profile's
// picture and sound, as ?w=320&h=180&fit=scale&fps=24&vb=400&ab=64.
var overrideParams = []string{"w", "h", "fit", "fps", "vb", "ab"}

// withOverrides is t with what q asks for, each value held within l: the
// size (rounded down to even), fit (scale: stretched to the size; crop43: the
// middle 4:3 of the picture, then stretched), the most frames a second, and
// the video and audio kbit/s. A value that is not a number, or not a fit, is
// left out. Codec, H.264 profile, slices and the rest stay the profile's.
func withOverrides(t config.Transcode, q url.Values, l config.Limits) config.Transcode {
	num := func(name string, lo, hi int, to *int) bool {
		n, err := strconv.Atoi(q.Get(name))
		if errors.Is(err, strconv.ErrRange) && n > 0 {
			n, err = hi, nil // too many digits for an int: the most
		}
		if err != nil || n < 1 {
			return false
		}
		*to = min(max(n, lo), hi)
		return true
	}
	if num("w", 16, l.MaxWidth, &t.MaxWidth) {
		t.MaxWidth &^= 1
	}
	if num("h", 16, l.MaxHeight, &t.MaxHeight) {
		t.MaxHeight &^= 1
	}
	num("fps", 1, l.FPS, &t.FPS)
	num("vb", 16, l.VideoKbps, &t.VideoKbps)
	num("ab", 8, l.AudioKbps, &t.AudioKbps)
	switch q.Get("fit") {
	case "scale":
		t.ExactSize, t.Crop = true, ""
	case "crop43":
		t.ExactSize, t.Crop = true, "4:3"
	}
	return t
}

// describeTranscode says what a conversion makes, for the X-Transcode header:
// "320x180 fit=scale fps=24 vb=400 ab=64". For fit=inside, a profile that
// fits the picture inside its size rather than stretching it, the size is that
// box: the picture's own shape, which ffmpeg learns only once it reads the
// stream, decides how much of it is used.
func describeTranscode(t config.Transcode) string {
	w, h, fit := t.MaxWidth, t.MaxHeight, "inside"
	if t.ExactSize {
		w, h, fit = evenCeil(w), evenCeil(h), "scale"
	}
	if a, b, ok := t.CropShape(); ok {
		fit = fmt.Sprintf("crop%d%d", a, b)
	}
	return fmt.Sprintf("%dx%d fit=%s fps=%d vb=%d ab=%d", w, h, fit, t.FPS, t.VideoKbps, t.AudioKbps)
}

type capsMax struct {
	W   int `json:"w"`
	H   int `json:"h"`
	FPS int `json:"fps"`
	VB  int `json:"vb"`
	AB  int `json:"ab"`
}

// apiCaps tells a device which overrides its stream requests may carry, and
// the most each may ask for.
func (s *Server) apiCaps(w http.ResponseWriter, r *http.Request) {
	l := s.cfg.Settings.Limits()
	writeJSON(w, http.StatusOK, struct {
		Overrides []string `json:"overrides"`
		Max       capsMax  `json:"max"`
	}{overrideParams, capsMax{l.MaxWidth, l.MaxHeight, l.FPS, l.VideoKbps, l.AudioKbps}})
}
