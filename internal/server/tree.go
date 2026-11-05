package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
)

// view is who is asking: the address the client used to reach this server, and
// how it is to be served.
type view struct {
	host string
	name string // the matched client, or empty for the default profile
	prof config.Profile
}

func (s *Server) viewOf(r *http.Request) view {
	name, prof := s.profileFor(r)
	return view{host: s.hostOf(r), name: name, prof: prof}
}

func (s *Server) browse(ctx context.Context, v view, args map[string]string) ([][2]string, error) {
	id := args["ObjectID"]
	start, err1 := atoiOr(args["StartingIndex"], 0)
	count, err2 := atoiOr(args["RequestedCount"], 0)
	if err1 != nil || err2 != nil || start < 0 || count < 0 {
		return nil, errInvalidArgs
	}
	var objs []object
	total := 1
	switch args["BrowseFlag"] {
	case "BrowseMetadata":
		o, err := s.lookup(ctx, v, id)
		if err != nil {
			return nil, err
		}
		objs = []object{o}
	case "BrowseDirectChildren":
		all, err := s.children(ctx, v, id)
		if err != nil {
			return nil, err
		}
		total = len(all)
		if start > len(all) {
			start = len(all)
		}
		all = all[start:]
		if count > 0 && count < len(all) {
			all = all[:count]
		}
		objs = all
	default:
		return nil, errInvalidArgs
	}
	return [][2]string{
		{"Result", didl(objs)},
		{"NumberReturned", strconv.Itoa(len(objs))},
		{"TotalMatches", strconv.Itoa(total)},
		{"UpdateID", s.systemUpdateID()},
	}, nil
}

func atoiOr(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(s)
}

// Object ids. Folders: live, fav, sources, src/<source>, rec (TV Shows),
// recgroup/<group id>, movies, recnow. Items are the folder's id, a slash and the channel number or
// file id, so one item in two folders has two ids; srcch/<source>/<number> is a
// channel in a source's folder.
const (
	idRoot       = "0"
	idLive       = "live"
	idFavorites  = "fav"
	idSources    = "sources"
	idRecordings = "rec"
	idRecNow     = "recnow"
	idRecent     = "recent"
	idMovies     = "movies"
)

// movieGroup is the group the DVR files every movie under. The movies get a
// folder of their own at the top, beside the shows.
const movieGroup = "movies"

func (s *Server) children(ctx context.Context, v view, id string) ([]object, error) {
	switch {
	case id == idRoot:
		var out []object
		for _, f := range []string{idLive, idFavorites, idRecent, idRecordings, idMovies, idRecNow, idSources} {
			o, err := s.lookup(ctx, v, f)
			if err == errNoSuchObject {
				continue // no favorites, no movies, or nothing recording
			}
			if err != nil {
				return nil, err
			}
			out = append(out, o)
		}
		return out, nil
	case id == idLive, id == idFavorites:
		return s.liveItems(ctx, v, id)
	case id == idSources:
		return s.sourceFolders(ctx, v)
	case strings.HasPrefix(id, "src/"):
		return s.sourceItems(ctx, v, strings.TrimPrefix(id, "src/"))
	case id == idRecordings:
		folders, _, err := s.recordingTree(ctx, v)
		return folders, err
	case id == idRecNow:
		return s.recordingNow(ctx, v)
	case id == idRecent:
		return s.recentRecordings(ctx, v)
	case id == idMovies:
		return s.movies(ctx, v)
	case strings.HasPrefix(id, "recgroup/"):
		_, items, err := s.recordingTree(ctx, v)
		if err != nil {
			return nil, err
		}
		var out []object
		for _, o := range items {
			if o.Parent == id {
				out = append(out, o)
			}
		}
		if out == nil {
			return nil, errNoSuchObject
		}
		return out, nil
	case strings.HasPrefix(id, "live/"), strings.HasPrefix(id, "fav/"), strings.HasPrefix(id, "srcch/"),
		strings.HasPrefix(id, "rec/"), strings.HasPrefix(id, "recnow/"), strings.HasPrefix(id, "recent/"),
		strings.HasPrefix(id, "movies/"):
		return nil, nil // items have no children
	}
	return nil, errNoSuchObject
}

func (s *Server) lookup(ctx context.Context, v view, id string) (object, error) {
	folder := func(title string, kids func() ([]object, error)) (object, error) {
		items, err := kids()
		if err != nil {
			return object{}, err
		}
		return object{ID: id, Parent: idRoot, Title: title, Folder: true, Children: len(items), Art: s.folderArt(v, id)}, nil
	}
	switch {
	case id == idRoot:
		kids, err := s.children(ctx, v, idRoot)
		if err != nil {
			return object{}, err
		}
		return object{ID: idRoot, Parent: "-1", Title: s.cfg.Name, Folder: true, Children: len(kids)}, nil
	case id == idLive:
		return folder("Live TV", func() ([]object, error) { return s.liveItems(ctx, v, idLive) })
	case id == idFavorites:
		o, err := folder("Favorites", func() ([]object, error) { return s.liveItems(ctx, v, idFavorites) })
		if err == nil && o.Children == 0 {
			err = errNoSuchObject
		}
		return o, err
	case id == idSources:
		return folder("Channels by source", s.sourceFoldersFn(ctx, v))
	case strings.HasPrefix(id, "src/"):
		folders, err := s.sourceFolders(ctx, v)
		if err != nil {
			return object{}, err
		}
		return find(folders, id)
	case id == idRecordings:
		return folder("TV Shows", func() ([]object, error) {
			folders, _, err := s.recordingTree(ctx, v)
			return folders, err
		})
	case id == idRecent:
		o, err := folder("Recent recordings", func() ([]object, error) { return s.recentRecordings(ctx, v) })
		if err == nil && o.Children == 0 {
			err = errNoSuchObject
		}
		return o, err
	case id == idMovies:
		o, err := folder("Movies", func() ([]object, error) { return s.movies(ctx, v) })
		if err == nil && o.Children == 0 {
			err = errNoSuchObject
		}
		return o, err
	case id == idRecNow:
		o, err := folder("Recording now", func() ([]object, error) { return s.recordingNow(ctx, v) })
		if err == nil && o.Children == 0 {
			err = errNoSuchObject
		}
		return o, err
	case strings.HasPrefix(id, "live/"), strings.HasPrefix(id, "fav/"):
		parent := idLive
		if strings.HasPrefix(id, "fav/") {
			parent = idFavorites
		}
		items, err := s.liveItems(ctx, v, parent)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	case strings.HasPrefix(id, "srcch/"):
		source, _, ok := strings.Cut(strings.TrimPrefix(id, "srcch/"), "/")
		if !ok {
			return object{}, errNoSuchObject
		}
		items, err := s.sourceItems(ctx, v, source)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	case strings.HasPrefix(id, "recgroup/"):
		folders, _, err := s.recordingTree(ctx, v)
		if err != nil {
			return object{}, err
		}
		return find(folders, id)
	case strings.HasPrefix(id, "rec/"):
		_, items, err := s.recordingTree(ctx, v)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	case strings.HasPrefix(id, "movies/"):
		items, err := s.movies(ctx, v)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	case strings.HasPrefix(id, "recnow/"):
		items, err := s.recordingNow(ctx, v)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	case strings.HasPrefix(id, "recent/"):
		items, err := s.recentRecordings(ctx, v)
		if err != nil {
			return object{}, err
		}
		return find(items, id)
	}
	return object{}, errNoSuchObject
}

// bookmarkID is the id a resume point is kept under: one recording is in
// several folders, under an id in each, and resumes the same from all of them.
func bookmarkID(id string) string {
	for _, p := range []string{"recent/", "recnow/", "movies/"} {
		if rest, ok := strings.CutPrefix(id, p); ok {
			return "rec/" + rest
		}
	}
	return id
}

// folderArt is the address of a top folder's icon, as the client reaches it.
func (s *Server) folderArt(v view, id string) string {
	return "http://" + v.host + "/icons/folder-" + id + ".png"
}

func find(objs []object, id string) (object, error) {
	for _, o := range objs {
		if o.ID == id {
			return o, nil
		}
	}
	return object{}, errNoSuchObject
}

// channelObject is a channel as a TV item in a folder. device is the source to
// play it from, or empty for whichever the DVR picks.
func (s *Server) channelObject(v view, ch dvr.Channel, id, parent, device, show string) object {
	title := ch.Number + " " + ch.Name
	if show != "" {
		title += " - " + show
	}
	q := url.Values{}
	if device != "" {
		q.Set("dev", device)
	}
	res := s.resource(v, dvr.LivePath(device, ch.Number), "/stream/live/"+url.PathEscape(ch.Number), q, false, liveProtocol)
	return object{
		ID: id, Parent: parent, Title: title, Class: classLive,
		URL: res.url, Protocol: res.protocol, Art: ch.Logo, ChannelNr: ch.Number, ChannelName: ch.Name,
		// A Samsung shows an item without a date as 1969.
		Date: time.Now(),
	}
}

// liveItems lists the channels in the Live TV folder, or just the favorites.
func (s *Server) liveItems(ctx context.Context, v view, parent string) ([]object, error) {
	chs, err := s.cfg.DVR.Channels(ctx)
	if err != nil {
		return nil, err
	}
	// Without the guide the channels still play, just without show titles.
	now, err := s.cfg.DVR.NowPlaying(ctx)
	if err != nil {
		s.cfg.Logf("guide: %v", err)
	}
	out := make([]object, 0, len(chs))
	for _, ch := range chs {
		if parent == idFavorites && !ch.Favorited {
			continue
		}
		out = append(out, s.channelObject(v, ch, parent+"/"+ch.Number, parent, "", now[ch.Number]))
	}
	return out, nil
}

func (s *Server) sourceFoldersFn(ctx context.Context, v view) func() ([]object, error) {
	return func() ([]object, error) { return s.sourceFolders(ctx, v) }
}

// sourceFolders is one folder for each source (tuner, M3U list...) that has
// channels, by name.
func (s *Server) sourceFolders(ctx context.Context, v view) ([]object, error) {
	all, err := s.cfg.DVR.AllChannels(ctx)
	if err != nil {
		return nil, err
	}
	count := map[string]int{}
	for _, ch := range all {
		count[ch.Source]++
	}
	var out []object
	for name, n := range count {
		title := name
		if title == "" {
			title = "Other"
		}
		out = append(out, object{ID: "src/" + url.PathEscape(name), Parent: idSources, Title: title, Folder: true, Children: n, Art: s.folderArt(v, idSources)})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out, nil
}

// sourceItems lists one source's channels, played from that source.
func (s *Server) sourceItems(ctx context.Context, v view, escaped string) ([]object, error) {
	source, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, errNoSuchObject
	}
	all, err := s.cfg.DVR.AllChannels(ctx)
	if err != nil {
		return nil, err
	}
	now, err := s.cfg.DVR.NowPlaying(ctx)
	if err != nil {
		s.cfg.Logf("guide: %v", err)
	}
	var out []object
	for _, ch := range all {
		if ch.Source == source {
			out = append(out, s.channelObject(v, ch, "srcch/"+escaped+"/"+ch.Number, "src/"+escaped, ch.SourceID, now[ch.Number]))
		}
	}
	if out == nil {
		return nil, errNoSuchObject
	}
	return out, nil
}

// recordingObject is a recording as a TV item. The breaks are cut out when the
// profile says so and the DVR has marked them.
func (s *Server) recordingObject(v view, f dvr.Recording, id, parent string) object {
	cut := v.prof.Ads == config.AdsCut && s.adFree(f)
	bm := s.cfg.Bookmarks.Get("rec/" + string(f.ID))
	if bm == 0 && f.PlaybackTime > 0 {
		// Where the Channels apps left off, on the TV's clock.
		if cut {
			bm = int(f.AdFree(f.PlaybackTime))
		} else {
			bm = int(f.PlaybackTime)
		}
	}
	img := f.Thumbnail
	if img == "" {
		img = f.Airing.Image
	}
	if img == "" {
		// A recording without guide data has no picture, and some TVs (an LG)
		// leave such an item out of the list: give it the bridge's icon.
		img = "http://" + v.host + "/icons/logo120.png"
	}
	eid := url.PathEscape(string(f.ID))
	bridge := "/stream/rec/" + eid
	if cut {
		bridge = "/stream/adfree/" + eid
	}
	res := s.resource(v, dvr.RecordingPath(string(f.ID)), bridge, nil, cut, recProtocol)
	o := object{
		ID: id, Parent: parent, Title: recordingTitle(f), Class: classRecorded,
		URL: res.url, Protocol: res.protocol, Duration: f.Duration, Art: img, Description: f.Airing.Summary,
		Date: f.Created(), Bookmark: bm,
	}
	if cut {
		o.Duration = f.AdFree(-1)
	}
	return o
}

// recordingTree is the show folders and the recordings in them. The movies
// are left out: they have a folder of their own.
func (s *Server) recordingTree(ctx context.Context, v view) (folders, items []object, err error) {
	files, err := s.cfg.DVR.Recordings(ctx)
	if err != nil {
		return nil, nil, err
	}
	groups, err := s.cfg.DVR.Groups(ctx)
	if err != nil {
		s.cfg.Logf("groups: %v", err)
	}
	names := map[string]string{}
	art := map[string]string{}
	for _, g := range groups {
		names[string(g.ID)], art[string(g.ID)] = g.Name, g.Image
	}
	count := map[string]int{}
	var order []string
	for _, f := range files {
		gid := string(f.GroupID)
		if gid == movieGroup {
			continue
		}
		if gid == "" {
			gid = "_" + f.Airing.Title // no folder from the DVR: group by show title
		}
		if count[gid] == 0 {
			order = append(order, gid)
			if names[gid] == "" {
				names[gid] = f.Airing.Title
			}
			if art[gid] == "" {
				art[gid] = f.Airing.Image
			}
		}
		count[gid]++
		items = append(items, s.recordingObject(v, f, "rec/"+string(f.ID), "recgroup/"+gid))
	}
	for _, gid := range order {
		title := names[gid]
		if title == "" {
			title = "Other"
		}
		a := art[gid]
		if a == "" {
			a = s.folderArt(v, idRecordings)
		}
		folders = append(folders, object{ID: "recgroup/" + gid, Parent: idRecordings, Title: title, Folder: true, Children: count[gid], Art: a})
	}
	sort.SliceStable(folders, func(i, j int) bool { return strings.ToLower(folders[i].Title) < strings.ToLower(folders[j].Title) })
	return folders, items, nil
}

// movies lists the recorded movies, newest first, each by its title alone.
func (s *Server) movies(ctx context.Context, v view) ([]object, error) {
	files, err := s.cfg.DVR.Recordings(ctx)
	if err != nil {
		return nil, err
	}
	var out []object
	for _, f := range files {
		if string(f.GroupID) != movieGroup {
			continue
		}
		o := s.recordingObject(v, f, "movies/"+string(f.ID), idMovies)
		if f.Airing.Title != "" {
			o.Title = f.Airing.Title
		}
		out = append(out, o)
	}
	return out, nil
}

// recordingNow lists the recordings the DVR has not finished, newest first.
// They are growing files, so they play whole, from the start.
func (s *Server) recordingNow(ctx context.Context, v view) ([]object, error) {
	files, err := s.cfg.DVR.Recordings(ctx)
	if err != nil {
		return nil, err
	}
	var out []object
	for _, f := range files {
		if f.Completed {
			continue
		}
		o := s.recordingObject(v, f, "recnow/"+string(f.ID), idRecNow)
		o.Title = f.Airing.Title + " " + f.Created().Local().Format("15:04")
		if f.Airing.EpisodeTitle != "" {
			o.Title += " " + f.Airing.EpisodeTitle
		}
		out = append(out, o)
	}
	return out, nil
}

// recentRecordings lists the recordings that started within the last
// RecentHours hours, newest first, whatever the show: a TV has no other way to
// see what is new. Each is titled with its show, since they are not in one.
func (s *Server) recentRecordings(ctx context.Context, v view) ([]object, error) {
	files, err := s.cfg.DVR.Recordings(ctx)
	if err != nil {
		return nil, err
	}
	since := s.now().Add(-time.Duration(s.cfg.Settings.RecentHours()) * time.Hour)
	var out []object
	for _, f := range files { // newest first
		if f.Created().Before(since) {
			break
		}
		o := s.recordingObject(v, f, "recent/"+string(f.ID), idRecent)
		if t := f.Airing.Title; t != "" && !strings.HasPrefix(o.Title, t) {
			o.Title = t + " - " + o.Title
		}
		out = append(out, o)
	}
	return out, nil
}

// recordingTitle names an episode by season, episode and title, or by date
// when the guide gave neither.
func recordingTitle(f dvr.Recording) string {
	a := f.Airing
	var parts []string
	if a.Season > 0 && a.Episode > 0 {
		parts = append(parts, fmt.Sprintf("S%02dE%02d", a.Season, a.Episode))
	}
	if a.EpisodeTitle != "" {
		parts = append(parts, a.EpisodeTitle)
	}
	if len(parts) == 0 {
		parts = append(parts, a.Title)
		if f.CreatedAt > 0 {
			parts = append(parts, f.Created().Format("2006-01-02"))
		}
	}
	return strings.Join(parts, " ")
}

type resource struct{ url, protocol string }

// resource is where a client plays an item and how it is described: from the
// DVR itself, through this server, or converted here. throughBridge forces the
// bridge, for streams only it can make (recordings with their breaks cut).
func (s *Server) resource(v view, dvrPath, bridgePath string, q url.Values, throughBridge bool, protocol string) resource {
	via := func() string {
		q := cloneValues(q)
		if v.name != "" {
			q.Set("profile", v.name) // the stream request may look different from the browse
		}
		u := "http://" + v.host + bridgePath
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		return u
	}
	switch {
	case v.prof.Mode == config.ModeTranscode:
		return resource{via(), transcodeProtocol}
	case throughBridge || v.prof.Mode == config.ModeProxy:
		return resource{via(), protocol}
	}
	return resource{s.dvrPublic(v.host) + dvrPath, protocol}
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = v
	}
	return out
}

func (s *Server) dvrPublic(host string) string {
	if s.cfg.DVRPublic != "" {
		return strings.TrimRight(s.cfg.DVRPublic, "/")
	}
	u := *s.cfg.DVR.Base
	if isLoopback(u.Hostname()) {
		h, _, err := net.SplitHostPort(host)
		if err != nil {
			h = host
		}
		port := u.Port()
		if port == "" {
			port = "80"
		}
		u.Host = net.JoinHostPort(strings.Trim(h, "[]"), port)
	}
	return u.String()
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
