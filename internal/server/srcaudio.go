package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"dlna/internal/config"
)

// Sound alone from a DLNA source, as the DVR's Audio folder: the source's
// tree again, each video as its sound in the profile's audio format, for
// players without a screen. Its ids are the source's after listen/; listen
// itself is the source's root. A client set to video does not see it, and
// one set to sound sees it alone.

// remoteID is the source's id of an object of the Audio folder.
func remoteID(id string) (string, bool) {
	if id == idListen {
		return idRoot, true
	}
	rid, ok := strings.CutPrefix(id, idListen+"/")
	return rid, ok && rid != ""
}

// soundID is the Audio folder's id of an object of the source.
func soundID(rid string) string {
	if rid == idRoot {
		return idListen
	}
	return idListen + "/" + rid
}

// showsSound says whether the client sees the Audio folder.
func (v view) showsSound() bool {
	return v.prof.Kind != config.KindVideo
}

// audioFolder is the Audio folder, with as many children as the source's root.
func (s *Server) audioFolder(v view, children int) object {
	return object{ID: idListen, Parent: idRoot, Title: "Audio", Folder: true, Children: children, Art: s.folderArt(v, idListen)}
}

// soundChildren is a folder of the source as the Audio folder shows it.
func (s *Server) soundChildren(ctx context.Context, v view, rid string, start, count int) ([]object, int, error) {
	if !v.showsSound() {
		return nil, 0, errNoSuchObject
	}
	objs, total, err := s.sourceList(ctx, v, "Browse", [][2]string{{"ObjectID", rid}, {"BrowseFlag", "BrowseDirectChildren"}}, start, count, true)
	if rid == idRoot {
		for i := range objs {
			objs[i].Parent = idListen
		}
	}
	return objs, total, err
}

// asSound is an object of the source as the Audio folder shows it: a folder or
// a song as it is, a video as its sound, a live one as a broadcast. Anything
// else is left out.
func (s *Server) asSound(v view, o object) (object, bool) {
	o.ID, o.Parent = soundID(o.ID), soundID(o.Parent)
	switch {
	case o.Folder, strings.HasPrefix(o.Class, "object.item.audioItem"):
		return o, true
	case !strings.HasPrefix(o.Class, "object.item.videoItem"):
		return o, false
	}
	if strings.HasPrefix(o.Class, "object.item.videoItem.videoBroadcast") && o.Duration == 0 {
		o.Class = classAudioLive
	} else {
		o.Class = "object.item.audioItem"
	}
	mime, features := audioFeatures(v.prof.Audio.Format)
	o.URL = s.bridgeURL(v, "/stream/srcaudio/"+url.PathEscape(strings.TrimPrefix(o.ID, idListen+"/")), nil)
	o.Protocol, o.ResAttrs = "http-get:*:"+mime+":"+features, nil
	return o, true
}

// streamSourceSound plays the sound of an item of the source to a client, as
// its profile's Audio says; a broadcast channel's from the feed its other
// clients share.
func (s *Server) streamSourceSound(w http.ResponseWriter, r *http.Request) {
	it, ok := s.streamItem(w, r)
	if !ok {
		return
	}
	if !strings.HasPrefix(it.class, "object.item.videoItem") && !strings.HasPrefix(it.class, "object.item.audioItem") {
		http.NotFound(w, r)
		return
	}
	name, prof := s.profileFor(r)
	if it.live() {
		feed := feedReq{url: it.url}
		s.convertAudio(w, r, name, prof, it.title, "pipe:0", &feed)
		return
	}
	s.convertAudio(w, r, name, prof, it.title, it.url, nil)
}
