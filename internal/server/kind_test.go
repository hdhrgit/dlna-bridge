package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
)

func apiBrowse(t *testing.T, base, query string) []string {
	resp, err := http.Get(base + "/api/browse?" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list []struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range list {
		ids = append(ids, e.ID)
	}
	return ids
}

// A client set to video is not shown the Listen folders, one set to sound is
// shown nothing else, and both (the default) sees all, at the top and when it
// asks for a folder by id.
func TestKinds(t *testing.T) {
	prof := func(kind string) config.Profile {
		return config.Profile{Kind: kind, Mode: config.ModeProxy, Playback: config.PlaybackKeep}
	}
	s := newServerWith(t, config.Config{Default: prof(""), Clients: []config.Client{
		{Name: "pictures", Profile: prof(config.KindVideo)},
		{Name: "speaker", Profile: prof(config.KindSound)},
		{Name: "all", Profile: prof(config.KindBoth)},
	}})
	all := "live fav recent rec movies recnow sources listen listensrc"
	for query, want := range map[string]string{
		"":                 all,
		"profile=all":      all,
		"profile=pictures": "live fav recent rec movies recnow sources",
		"profile=speaker":  "listen listensrc",
		// A folder that is not shown is not there either.
		"profile=pictures&id=listen":    "",
		"profile=pictures&id=listensrc": "",
		"profile=speaker&id=live":       "",
		"profile=speaker&id=rec":        "",
		"profile=speaker&id=sources":    "",
		"profile=speaker&id=listensrc":  "lsrc/FAST lsrc/Tuner%20A",
	} {
		if got := strings.Join(apiBrowse(t, s.URL, query), " "); got != want {
			t.Errorf("%q: got %q, want %q", query, got, want)
		}
	}
	if got := apiBrowse(t, s.URL, "profile=speaker&id=listen"); len(got) != 3 || !strings.HasPrefix(got[0], "listen/") {
		t.Errorf("speaker's Listen: %v", got)
	}
}

// Listen reads the channel's feed, the whole channel from the DVR, and
// converts its sound as the profile says.
func TestListenFromTheDVR(t *testing.T) {
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ff, []byte("#!/bin/sh\necho \"$@\" >> "+dir+"/args\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := fakeDVR(t)
	c, err := dvr.New(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	aud := func(format string, kbps int) config.Profile {
		return config.Profile{Kind: config.KindSound, Mode: config.ModeProxy, Audio: config.Audio{Format: format, Kbps: kbps, Channels: 2}}
	}
	settings := mustSettings(t, config.Config{Clients: []config.Client{
		{Name: "aac", Profile: aud(config.AudioAAC, 96)},
		{Name: "mp3", Profile: aud(config.AudioMP3, 192)},
		{Name: "copy", Profile: aud(config.AudioCopy, 128)},
	}})
	srv := httptest.NewServer(New(Config{DVR: c, FFmpeg: ff, Settings: settings, Logf: func(string, ...any) {}}).Handler())
	defer srv.Close()
	for query, want := range map[string]string{
		"profile=aac":         "-i " + d.URL + "/devices/ANY/channels/5000/stream.mpg -map 0:v:0? -map 0:a:0? -c copy",
		"profile=aac ":        "-i pipe:0 -map 0:a:0 -vn -c:a aac -f adts -b:a 96k -ac 2 pipe:1",
		"profile=mp3":         "-c:a libmp3lame -f mp3 -b:a 192k -ac 2 pipe:1",
		"profile=copy":        "-map 0:a:0 -vn -c:a copy -f mpegts pipe:1",
		"profile=copy&dev=A1": "-i " + d.URL + "/devices/A1/channels/5000/stream.mpg ",
	} {
		os.Remove(filepath.Join(dir, "args"))
		resp, err := http.Get(srv.URL + "/stream/audio/5000?" + strings.TrimSpace(query))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		// The feed's ffmpeg runs beside the client's: wait for both.
		var args []byte
		for i := 0; i < 200 && strings.Count(string(args), "\n") < 2; i++ {
			time.Sleep(10 * time.Millisecond)
			args, _ = os.ReadFile(filepath.Join(dir, "args"))
		}
		if !strings.Contains(string(args), want) {
			t.Errorf("%s: no %q in %s", query, want, args)
		}
		if strings.Contains(query, "copy") && strings.Contains(string(args), "-b:a") {
			t.Errorf("%s: pass through is re-encoded: %s", query, args)
		}
	}
}
