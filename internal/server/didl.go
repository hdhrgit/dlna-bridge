package server

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// object is one folder or video in the tree the TVs browse.
type object struct {
	ID, Parent string
	Title      string
	Folder     bool
	Children   int // folders only

	Class       string // items only
	URL         string
	Protocol    string // the res protocolInfo
	Duration    float64
	Art         string
	Description string
	Date        time.Time
	ChannelNr   string
	ChannelName string
	Bookmark    int // seconds; Samsung TVs offer to resume from here
	Watched     bool
}

// DLNA flags: streaming transfer mode, background transfer, connection stall
// and DLNA 1.5. Live streams cannot seek (OP=00); recordings take byte ranges
// (OP=01).
const (
	dlnaFlags    = "DLNA.ORG_FLAGS=01700000000000000000000000000000"
	liveFeatures = "DLNA.ORG_OP=00;DLNA.ORG_CI=0;" + dlnaFlags
	recFeatures  = "DLNA.ORG_OP=01;DLNA.ORG_CI=0;" + dlnaFlags
	videoMIME    = "video/mpeg"
	liveProtocol = "http-get:*:" + videoMIME + ":" + liveFeatures
	recProtocol  = "http-get:*:" + videoMIME + ":" + recFeatures
	// A recording with its breaks cut, for a client that seeks by time: no
	// length and no byte ranges (OP=10).
	timeProtocol = "http-get:*:" + videoMIME + ":" + timeFeatures
	timeFeatures = "DLNA.ORG_OP=10;DLNA.ORG_CI=0;" + dlnaFlags
	tsMIME       = "video/mp2t"
	// A converted stream is a live MPEG-TS: no length, no seeking.
	transcodeProtocol = "http-get:*:" + tsMIME + ":" + transcodeFeatures
	transcodeFeatures = "DLNA.ORG_OP=00;DLNA.ORG_CI=1;" + dlnaFlags
	// A converted recording for a client set to seek by time (OP=10).
	transcodeTimeProtocol = "http-get:*:" + tsMIME + ":" + transcodeTimeFeatures
	transcodeTimeFeatures = "DLNA.ORG_OP=10;DLNA.ORG_CI=1;" + dlnaFlags
	classLive             = "object.item.videoItem.videoBroadcast"
	classRecorded         = "object.item.videoItem"
)

const didlOpen = `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/" xmlns:sec="http://www.sec.co.kr/">`

func didl(objs []object) string {
	var b strings.Builder
	b.WriteString(didlOpen)
	for _, o := range objs {
		if o.Folder {
			fmt.Fprintf(&b, `<container id="%s" parentID="%s" restricted="1" searchable="1" childCount="%d">`, escape(o.ID), escape(o.Parent), o.Children)
			b.WriteString("<dc:title>" + escape(o.Title) + "</dc:title>")
			b.WriteString("<upnp:class>object.container.storageFolder</upnp:class>")
			if o.Art != "" {
				b.WriteString(albumArt(o.Art))
			}
			b.WriteString("</container>")
			continue
		}
		fmt.Fprintf(&b, `<item id="%s" parentID="%s" restricted="1">`, escape(o.ID), escape(o.Parent))
		b.WriteString("<dc:title>" + escape(o.Title) + "</dc:title>")
		b.WriteString("<upnp:class>" + o.Class + "</upnp:class>")
		if !o.Date.IsZero() {
			b.WriteString("<dc:date>" + o.Date.UTC().Format("2006-01-02T15:04:05") + "</dc:date>")
		}
		if o.Description != "" {
			b.WriteString("<dc:description>" + escape(o.Description) + "</dc:description>")
		}
		if o.Art != "" {
			b.WriteString(albumArt(o.Art))
		}
		// channelNr is an integer in the schema; leave out numbers like 4.1.
		if _, err := strconv.Atoi(o.ChannelNr); err == nil {
			b.WriteString("<upnp:channelNr>" + o.ChannelNr + "</upnp:channelNr>")
		}
		if o.ChannelName != "" {
			b.WriteString("<upnp:channelName>" + escape(o.ChannelName) + "</upnp:channelName>")
		}
		if o.Bookmark > 0 {
			fmt.Fprintf(&b, "<sec:dcmInfo>CREATIONDATE=%d,BM=%d</sec:dcmInfo>", o.Date.Unix(), o.Bookmark)
			b.WriteString("<upnp:lastPlaybackPosition>" + duration(float64(o.Bookmark)) + "</upnp:lastPlaybackPosition>")
		}
		if o.Class == classRecorded {
			n := 0
			if o.Watched {
				n = 1
			}
			fmt.Fprintf(&b, "<upnp:playbackCount>%d</upnp:playbackCount>", n)
		}
		b.WriteString(`<res protocolInfo="` + escape(o.Protocol) + `"`)
		if o.Duration > 0 {
			b.WriteString(` duration="` + duration(o.Duration) + `"`)
		}
		b.WriteString(">" + escape(o.URL) + "</res>")
		b.WriteString("</item>")
	}
	b.WriteString("</DIDL-Lite>")
	return b.String()
}

func albumArt(u string) string {
	profile := ""
	switch strings.ToLower(path.Ext(strings.SplitN(u, "?", 2)[0])) {
	case ".jpg", ".jpeg":
		profile = ` dlna:profileID="JPEG_TN"`
	case ".png":
		profile = ` dlna:profileID="PNG_TN"`
	}
	return "<upnp:albumArtURI" + profile + ">" + escape(u) + "</upnp:albumArtURI>"
}

// duration formats seconds as H:MM:SS.mmm.
func duration(sec float64) string {
	ms := int64(sec * 1000)
	return fmt.Sprintf("%d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
