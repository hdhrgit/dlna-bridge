package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
)

// Listen: the live channels as sound alone, for players without a screen (a
// kitchen speaker, a receiver). ffmpeg takes the channel's audio and sends it
// as the client's profile says: AAC, MP3, or the channel's own sound
// unconverted.

// Object ids: listen (every channel), listensrc (a folder per source),
// lsrc/<source>, and the items listen/<number>, lsrcch/<source>/<number>.
const (
	idListen        = "listen"
	idListenSources = "listensrc"
	classAudioLive  = "object.item.audioItem.audioBroadcast"
)

func audioFeatures(format string) (mime, features string) {
	switch format {
	case config.AudioMP3:
		return "audio/mpeg", "DLNA.ORG_PN=MP3;DLNA.ORG_OP=00;DLNA.ORG_CI=1;" + dlnaFlags
	case config.AudioCopy:
		return tsMIME, "DLNA.ORG_OP=00;DLNA.ORG_CI=0;" + dlnaFlags
	}
	return "audio/vnd.dlna.adts", "DLNA.ORG_PN=AAC_ADTS;DLNA.ORG_OP=00;DLNA.ORG_CI=1;" + dlnaFlags
}

// audioObject is a channel to listen to.
func (s *Server) audioObject(v view, ch dvr.Channel, id, parent, device, show string) object {
	title := ch.Number + " " + ch.Name
	if show != "" {
		title += " - " + show
	}
	q := url.Values{}
	if device != "" {
		q.Set("dev", device)
	}
	if v.name != "" {
		q.Set("profile", v.name)
	}
	u := "http://" + v.host + "/stream/audio/" + url.PathEscape(ch.Number)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	mime, features := audioFeatures(v.prof.Audio.Format)
	return object{
		ID: id, Parent: parent, Title: title, Class: classAudioLive,
		URL: u, Protocol: "http-get:*:" + mime + ":" + features,
		Art: s.channelArt(v, ch.Logo), ChannelNr: ch.Number, ChannelName: ch.Name, Date: time.Now(),
	}
}

// listenItems are the channels to listen to: all, or one source's.
func (s *Server) listenItems(ctx context.Context, v view, source string) ([]object, error) {
	var chs []dvr.Channel
	var err error
	if source == "" {
		chs, err = s.cfg.DVR.Channels(ctx)
	} else {
		chs, err = s.cfg.DVR.AllChannels(ctx)
	}
	if err != nil {
		return nil, err
	}
	now, _ := s.cfg.DVR.NowPlaying(ctx)
	var out []object
	esc := url.PathEscape(source)
	for _, ch := range chs {
		switch {
		case source == "":
			out = append(out, s.audioObject(v, ch, idListen+"/"+ch.Number, idListen, "", now[ch.Number]))
		case ch.Source == source:
			out = append(out, s.audioObject(v, ch, "lsrcch/"+esc+"/"+ch.Number, "lsrc/"+esc, ch.SourceID, now[ch.Number]))
		}
	}
	if source != "" && out == nil {
		return nil, errNoSuchObject
	}
	return out, nil
}

// listenSourceFolders is a folder per source, as Channels by source.
func (s *Server) listenSourceFolders(ctx context.Context, v view) ([]object, error) {
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
		out = append(out, object{ID: "lsrc/" + url.PathEscape(name), Parent: idListenSources, Title: title, Folder: true, Children: n, Art: s.folderArt(v, idListen)})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out, nil
}

// audioArgs has ffmpeg take a stream's sound alone, as a says. The input is
// the DVR's live HLS of the sound, started at its newest segment.
func audioArgs(a config.Audio, input, userAgent string) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if userAgent != "" {
		args = append(args, "-user_agent", userAgent)
	}
	args = append(args, "-live_start_index", "-1", "-i", input, "-map", "0:a:0", "-vn")
	switch a.Format {
	case config.AudioCopy:
		return append(args, "-c:a", "copy", "-f", "mpegts", "pipe:1")
	case config.AudioMP3:
		args = append(args, "-c:a", "libmp3lame", "-f", "mp3")
	default:
		args = append(args, "-c:a", "aac", "-f", "adts")
	}
	return append(args, "-b:a", strconv.Itoa(a.Kbps)+"k", "-ac", strconv.Itoa(a.Channels), "pipe:1")
}

// streamAudio plays a live channel's sound to a client.
func (s *Server) streamAudio(w http.ResponseWriter, r *http.Request) {
	number, dev := r.PathValue("number"), r.URL.Query().Get("dev")
	name, prof := s.profileFor(r)
	ch, found, err := s.cfg.DVR.Channel(r.Context(), number)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	mime, features := audioFeatures(prof.Audio.Format)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("TransferMode.DLNA.ORG", "Streaming")
	w.Header().Set("ContentFeatures.DLNA.ORG", features)
	if r.Method == http.MethodHead {
		return
	}
	ip := s.clientIP(r)
	what := fmt.Sprintf("%s %s (sound, %s)", number, ch.Name, prof.Audio.Format)
	limit := s.cfg.Settings.MaxTranscodes()
	id, ok := s.jobs.start(limit, name, ip, r.Header.Get("User-Agent"), what)
	if !ok {
		http.Error(w, fmt.Sprintf("already converting %d streams", limit), http.StatusServiceUnavailable)
		return
	}
	defer s.jobs.end(id)
	w = &countWriter{w, s.jobs.counter(id)}
	started := time.Now()
	s.cfg.Logf("audio: %s to %s (%s)", what, ip, name)
	// The DVR sends the sound alone, so no video crosses the network to be
	// thrown away here.
	_, msg, err := s.runFFmpeg(w, r, audioArgs(prof.Audio, s.cfg.DVR.SoundStreamURL(dev, number), s.agent()), false, stallLimit, what, nil)
	s.cfg.Logf("audio: %s to %s ended after %s: %v %s", what, ip, time.Since(started).Round(time.Second), err, msg)
}
