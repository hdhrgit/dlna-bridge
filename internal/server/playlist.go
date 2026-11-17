package server

import (
	"context"
	"strings"

	"dlna/internal/dvr"
	"dlna/internal/source"
)

// The live channels come from the DVR, or from an M3U playlist served in its
// place. A playlist's channels fill the same folders as the DVR's: Live TV,
// a folder for each group (as for each of the DVR's sources) and Audio. Its
// items are named by the channel's ID rather than its number, which a list
// can change.

// channelList is where the live channels come from.
type channelList interface {
	AllChannels(ctx context.Context) ([]dvr.Channel, error)
	Channels(ctx context.Context) ([]dvr.Channel, error)
	Channel(ctx context.Context, key string) (dvr.Channel, bool, error)
	NowPlaying(ctx context.Context) (map[string]string, error)
}

// channels is the DVR, or the playlist; nil while there is neither.
func (s *Server) channels() channelList {
	switch {
	case s.cfg.Playlist != nil:
		return playlistChannels{s.cfg.Playlist}
	case s.cfg.DVR != nil:
		return s.cfg.DVR
	}
	return nil
}

// playlistChannels are a playlist's entries as channels: the group is the
// source.
type playlistChannels struct{ p *source.Playlist }

func (c playlistChannels) AllChannels(context.Context) ([]dvr.Channel, error) {
	es := c.p.Entries()
	out := make([]dvr.Channel, 0, len(es))
	for _, e := range es {
		out = append(out, dvr.Channel{ID: e.ID, Number: e.Number, Name: e.Name, Logo: e.Logo, Source: e.Group})
	}
	return out, nil
}

func (c playlistChannels) Channels(ctx context.Context) ([]dvr.Channel, error) {
	return c.AllChannels(ctx)
}

func (c playlistChannels) Channel(_ context.Context, id string) (dvr.Channel, bool, error) {
	e, ok := c.p.Entry(id)
	return dvr.Channel{ID: e.ID, Number: e.Number, Name: e.Name, Logo: e.Logo, Source: e.Group}, ok, nil
}

// NowPlaying is empty: a playlist has no guide.
func (playlistChannels) NowPlaying(context.Context) (map[string]string, error) { return nil, nil }

// chKey is what names a channel in object ids and stream paths: its number on
// the DVR, its ID in a playlist.
func (s *Server) chKey(ch dvr.Channel) string {
	if s.cfg.Playlist != nil {
		return ch.ID
	}
	return ch.Number
}

// liveDirect is where a client set to Direct plays a channel: the DVR, or the
// playlist's own address for it.
func (s *Server) liveDirect(v view, ch dvr.Channel, device string) string {
	if s.cfg.Playlist != nil {
		e, _ := s.cfg.Playlist.Entry(ch.ID)
		return e.URL
	}
	return s.dvrPublic(v.host) + dvr.LivePath(device, ch.Number)
}

// liveFeed names the feed of a channel, by its key.
func (s *Server) liveFeed(key, device string) feedReq {
	if s.cfg.Playlist != nil {
		e, _ := s.cfg.Playlist.Entry(key)
		return feedReq{number: key, url: e.URL}
	}
	return feedReq{dev: device, number: key}
}

// serves says whether the tree has an id: with a playlist only the live
// channels and their sound do, and with no source yet only the root.
func (s *Server) serves(id string) bool {
	switch {
	case s.cfg.DVR != nil || id == idRoot:
		return true
	case s.cfg.Playlist == nil:
		return false
	}
	switch id {
	case idLive, idSources, idListen, idListenSources:
		return true
	}
	for _, p := range []string{"live/", "src/", "srcch/", "listen/", "lsrc/", "lsrcch/"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// WatchPlaylist reads the playlist again every hour, and tells the subscribed
// TVs when its channels changed.
func (s *Server) WatchPlaylist(ctx context.Context) {
	s.cfg.Playlist.Run(ctx, func() {
		s.changedFolders("playlist", []string{idRoot, idLive, idSources, idListen, idListenSources})
	})
}
