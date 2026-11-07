package server

import (
	"context"
	"strings"
	"time"

	"dlna/internal/config"
)

// Watched by others: what other clients are playing through the bridge now,
// to join. A channel joins live; a recording carries where the other client
// is in it as its resume point, so a TV that offers to resume starts there,
// and Play on a TV seeks there. A channel comes as sound too, for players
// without a screen.

const idOthers = "others"

// othersItems lists what clients other than v's are playing.
func (s *Server) othersItems(ctx context.Context, v view) ([]object, error) {
	var out []object
	seen := map[string]bool{}
	for _, w := range s.jobs.watched() {
		if w.IP == v.ip || w.Bytes < minWatchMB || seen[w.Object] {
			continue // the client itself, or a TV probing
		}
		seen[w.Object] = true
		who := w.Client
		if who == "" {
			who = w.IP
		}
		kind, rest, _ := strings.Cut(w.Object, "/")
		switch kind {
		case "live":
			number, dev, _ := strings.Cut(rest, "?")
			ch, found, err := s.cfg.DVR.Channel(ctx, number)
			if err != nil || !found {
				continue
			}
			now, _ := s.cfg.DVR.NowPlaying(ctx)
			o := s.channelObject(v, ch, idOthers+"/live/"+number, idOthers, dev, now[number])
			o.Title = who + ": " + o.Title
			out = append(out, o)
			a := s.audioObject(v, ch, idOthers+"/listen/"+number, idOthers, dev, now[number])
			a.Title = who + ": " + a.Title + " (sound)"
			out = append(out, a)
		case "rec", "adfree":
			f, found, err := s.cfg.DVR.Recording(ctx, rest)
			if err != nil || !found {
				continue
			}
			// Where the other client is, in the recording's own time.
			at := 0.0
			if kind == "rec" && f.FileSize > 0 {
				at = float64(w.Start+w.Bytes) / float64(f.FileSize) * f.Duration
			} else if plan, err := s.plans.get(ctx, f, s.cfg.DVR.File(f)); err == nil && plan.Size > 0 {
				at = f.RecordingTime(float64(w.Start+w.Bytes) / float64(plan.Size) * f.AdFree(-1))
			}
			at = max(min(at, f.Duration)-readAhead, 0)
			o := s.recordingObject(v, f, idOthers+"/rec/"+rest, idOthers)
			pos := at // on this client's clock: without the breaks if it gets them cut
			if v.prof.Ads == config.AdsCut && s.adFree(f) {
				pos = f.AdFree(at)
			}
			o.Bookmark = int(pos)
			title := o.Title
			if !strings.HasPrefix(title, f.Airing.Title) {
				title = f.Airing.Title + " " + title
			}
			o.Title = who + ": " + title + " (at " + duration(pos)[:len(duration(pos))-4] + ")"
			out = append(out, o)
		}
	}
	return out, nil
}

// joinAt is where Play on a TV seeks to after starting an item of Watched by
// others, 0 for none.
func joinAt(o object) time.Duration {
	if strings.HasPrefix(o.ID, idOthers+"/rec/") && o.Bookmark > 0 {
		return time.Duration(o.Bookmark) * time.Second
	}
	return 0
}
