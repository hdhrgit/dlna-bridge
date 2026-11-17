// dlna lists a Channels DVR's live channels and recordings on the LAN as a
// DLNA media server, so TVs without a Channels app can play them.
package main

import (
	"context"
	"crypto/sha1"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"dlna/internal/config"
	"dlna/internal/dvr"
	"dlna/internal/server"
	"dlna/internal/source"
	"dlna/internal/ssdp"
)

var version = "dev"

func main() {
	listen := flag.String("listen", envOr("DLNA_LISTEN", ":8200"), "address to listen on")
	dvrURL := flag.String("dvr", envOr("DLNA_DVR", "http://localhost:8089"), "address of the Channels DVR")
	src := flag.String("source", envOr("DLNA_SOURCE", ""), "serve this instead of -dvr: a server's address (host or host:port), or an http(s) URL")
	srcType := flag.String("source-type", envOr("DLNA_SOURCE_TYPE", source.Auto), "what -source is: auto (find out), channels, dlna or m3u")
	dvrPublic := flag.String("dvr-public", envOr("DLNA_DVR_PUBLIC", ""), "the DVR's address as the TVs reach it (default: -dvr, with localhost swapped for this machine's address)")
	name := flag.String("name", envOr("DLNA_NAME", "Channels DVR"), "name the TVs show")
	advertise := flag.String("advertise", envOr("DLNA_ADVERTISE", ""), "address to announce to the TVs (default: the one facing each TV)")
	advertisePort := flag.Int("advertise-port", envInt("DLNA_ADVERTISE_PORT", 0), "port to announce to the TVs, when they reach the server through a proxy on another one (default: the port of -listen)")
	proxies := flag.String("trusted-proxies", envOr("DLNA_TRUSTED_PROXIES", ""), "addresses or ranges of the proxies in front of this server, whose X-Forwarded-For and X-Forwarded-Host are believed")
	iface := flag.String("iface", envOr("DLNA_IFACE", ""), "network interface for discovery (default: the system's)")
	streams := flag.String("streams", envOr("DLNA_STREAMS", "proxy"), "first-run default for clients: proxy (through this server, works for every TV) or direct (play from the DVR, an optimization)")
	configFile := flag.String("config", envOr("DLNA_CONFIG", ""), "settings file the web page writes (default: config.json in -state)")
	keepAds := flag.Bool("keep-ads", envBool("DLNA_KEEP_ADS"), "first-run default: play recordings with their commercials in")
	state := flag.String("state", envOr("DLNA_STATE", ""), "directory for bookmarks.json (default: keep bookmarks in memory)")
	ffmpeg := flag.String("ffmpeg", envOr("DLNA_FFMPEG", "ffmpeg"), "ffmpeg for the tiny TV's streams")
	maxTranscodes := flag.Int("max-transcodes", envInt("DLNA_MAX_TRANSCODES", 2), "first-run default: conversions at once")
	debug := flag.Bool("debug", envBool("DLNA_DEBUG"), "log each SOAP request and the start of its response")
	maxStale := flag.Duration("dvr-max-stale", envDuration("DLNA_DVR_MAX_STALE", 15*time.Minute), "how old the DVR's channel and recording lists may be when it does not answer and they are served anyway")
	userAgent := flag.String("user-agent", envOr("DLNA_USER_AGENT", ""), "what the bridge calls itself to the DVR (default: dlna-bridge/<version>)")
	clientName := flag.String("client-name", envOr("DLNA_CLIENT_NAME", "DLNA Bridge"), "what the DVR calls the bridge in its client list and activity list, apart from the DLNA name the TVs see (empty: do not register)")
	planAhead := flag.Bool("plan-ahead", envOr("DLNA_PLAN_AHEAD", "true") != "false", "work out the cuts of each new recording once the DVR has marked its breaks, so its first play starts at once")
	planAheadBreaks := flag.Int("plan-ahead-breaks", envInt("DLNA_PLAN_AHEAD_BREAKS", 6), "plan ahead only new recordings with at least this many breaks; fewer are planned on first play, in well under a second; 0: plan none ahead")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *streams != config.ModeDirect && *streams != config.ModeProxy {
		fatal(fmt.Errorf("-streams must be direct or proxy, not %q", *streams))
	}
	switch *srcType {
	case source.Auto, source.Channels, source.DLNA, source.M3U:
	default:
		fatal(fmt.Errorf("-source-type must be auto, channels, dlna or m3u, not %q", *srcType))
	}
	if *srcType == source.M3U && !source.IsURL(*src) {
		fatal(fmt.Errorf("-source-type m3u needs an http(s) URL in -source, not %q", *src))
	}
	agent := "dlna-bridge/" + version
	if *userAgent != "" {
		agent = *userAgent
	}
	newDVR := func(addr string) *dvr.Client {
		client, err := dvr.New(addr)
		if err != nil {
			fatal(err)
		}
		client.MaxStale = *maxStale
		client.UserAgent = agent
		return client
	}
	bookmarks, err := server.OpenBookmarks(*state)
	if err != nil {
		fatal(err)
	}
	if *configFile == "" && *state != "" {
		*configFile = filepath.Join(*state, "config.json")
	}
	plansDir := ""
	if *state != "" {
		plansDir = filepath.Join(*state, "plans")
	}
	// The flags only set the start; once the page has saved the file, it wins.
	start := config.Config{MaxTranscodes: *maxTranscodes, Default: config.Profile{Mode: *streams, Ads: config.AdsCut}}
	if *keepAds {
		start.Default.Ads = config.AdsKeep
	}
	settings, err := config.Open(*configFile, start)
	if err != nil {
		fatal(err)
	}
	_, portStr, err := net.SplitHostPort(*listen)
	if err != nil {
		fatal(fmt.Errorf("-listen: %w", err))
	}
	port, _ := strconv.Atoi(portStr)
	if *advertisePort != 0 {
		port = *advertisePort
	}
	trusted, err := server.ParseProxies(*proxies)
	if err != nil {
		fatal(fmt.Errorf("-trusted-proxies: %w", err))
	}
	var ifi *net.Interface
	if *iface != "" {
		if ifi, err = net.InterfaceByName(*iface); err != nil {
			fatal(fmt.Errorf("-iface: %w", err))
		}
	}
	uuid := stableUUID(*name, *dvrURL)
	if *src != "" {
		uuid = stableUUID(*name, *src)
	}

	logs := server.NewLogBuffer(500)
	var adv *ssdp.Advertiser
	base := server.Config{
		Name: *name, UUID: uuid, Version: version,
		DVRPublic: *dvrPublic, Settings: settings, FFmpeg: *ffmpeg,
		TrustedProxies: trusted, SelfURL: "http://127.0.0.1:" + portStr, PlansDir: plansDir, PlanAheadBreaks: *planAheadBreaks, ClientName: *clientName, Debug: *debug, Bookmarks: bookmarks,
		Logf: logs.Logf, Logs: logs, ConfigPath: *configFile,
		Discovery: func() ssdp.Stats { return adv.Stats() },
		Source:    *src, UserAgent: agent,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// serve starts serving a source of a kind, at where, in place of what was
	// served before.
	var handler swapHandler
	serve := func(kind, where string) {
		cfg := base
		cfg.SourceType = kind
		if *src != "" {
			logs.Logf("source: %s is %s, at %s", *src, kindName[kind], where)
		}
		switch kind {
		case source.Channels:
			cfg.DVR = newDVR(where)
			srv := server.New(cfg)
			go srv.KeepRegistered(ctx)
			go srv.WatchChanges(ctx)
			go srv.WatchGuide(ctx)
			if *planAhead {
				go srv.PlanAhead(ctx)
			}
			handler.set(srv.Handler())
			return
		case source.M3U:
			cfg.Playlist = source.OpenPlaylist(ctx, where, source.CacheFile(*state, where), agent, logs.Logf)
			srv := server.New(cfg)
			go srv.WatchPlaylist(ctx)
			handler.set(srv.Handler())
		case source.DLNA:
			cfg.DLNA = source.NewServer(where, agent, logs.Logf)
			// Its own name, unless one was set.
			cfg.NameFromSource = os.Getenv("DLNA_NAME") == ""
			flag.Visit(func(f *flag.Flag) {
				if f.Name == "name" {
					cfg.NameFromSource = false
				}
			})
			srv := server.New(cfg)
			go srv.WatchSource(ctx)
			handler.set(srv.Handler())
		}
		logs.Logf("source: not a Channels DVR, so DLNA_DVR (%s) is not used", *dvrURL)
	}
	switch {
	case *src == "":
		serve(source.Channels, *dvrURL)
	case *srcType == source.Channels:
		serve(source.Channels, source.DVRAddress(*src))
	case *srcType == source.M3U, *srcType == source.DLNA:
		serve(*srcType, *src)
	default:
		kind, where, err := source.Detect(ctx, *src, agent)
		if err != nil && source.IsURL(*src) {
			// A playlist read before is served while its host is down.
			if _, serr := os.Stat(source.CacheFile(*state, *src)); serr == nil {
				kind, where, err = source.M3U, *src, nil
			}
		}
		if err == nil {
			serve(kind, where)
			break
		}
		// Nothing answers: an empty server until something does.
		logs.Logf("source: %v; looking again every minute", err)
		handler.set(server.New(base).Handler())
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Minute):
				}
				kind, where, err := source.Detect(ctx, *src, agent)
				if err == nil {
					serve(kind, where)
					return
				}
			}
		}()
	}
	hs := &http.Server{Addr: *listen, Handler: &handler, ReadHeaderTimeout: 10 * time.Second, ConnContext: server.ConnContext}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	adv = &ssdp.Advertiser{UUID: uuid, Port: port, Server: server.ServerHeader(version), Host: *advertise, Iface: ifi, Logf: logs.Logf}
	go func() {
		if err := adv.Run(ctx); err != nil {
			fatal(err)
		}
	}()

	if *src != "" {
		log.Printf("dlna %s: %q on %s for %s, settings in %q", version, *name, *listen, *src, *configFile)
	} else {
		log.Printf("dlna %s: %q on %s for the DVR at %s, settings in %q", version, *name, *listen, *dvrURL, *configFile)
	}
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the byebye go out
}

var kindName = map[string]string{
	source.Channels: "a Channels DVR",
	source.DLNA:     "a DLNA server",
	source.M3U:      "an M3U playlist",
}

// swapHandler serves with one handler and then another: the server of the
// source once it is found.
type swapHandler struct{ h atomic.Pointer[http.Handler] }

func (s *swapHandler) set(h http.Handler) { s.h.Store(&h) }

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

// stableUUID keeps the same id across restarts, so TVs do not list the server
// twice, without needing a state directory.
func stableUUID(parts ...string) string {
	h := sha1.New()
	host, _ := os.Hostname()
	fmt.Fprint(h, host)
	for _, p := range parts {
		fmt.Fprint(h, "\x00", p)
	}
	b := h.Sum(nil)
	b[6] = b[6]&0x0f | 0x50
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

func envBool(key string) bool {
	v, _ := strconv.ParseBool(os.Getenv(key))
	return v
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dlna:", err)
	os.Exit(1)
}
