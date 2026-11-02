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
	"strconv"
	"syscall"
	"time"

	"dlna/internal/dvr"
	"dlna/internal/server"
	"dlna/internal/ssdp"
)

var version = "dev"

func main() {
	listen := flag.String("listen", envOr("DLNA_LISTEN", ":8200"), "address to listen on")
	dvrURL := flag.String("dvr", envOr("DLNA_DVR", "http://localhost:8089"), "address of the Channels DVR")
	dvrPublic := flag.String("dvr-public", envOr("DLNA_DVR_PUBLIC", ""), "the DVR's address as the TVs reach it (default: -dvr, with localhost swapped for this machine's address)")
	name := flag.String("name", envOr("DLNA_NAME", "Channels DVR"), "name the TVs show")
	advertise := flag.String("advertise", envOr("DLNA_ADVERTISE", ""), "address to announce to the TVs (default: the one facing each TV)")
	iface := flag.String("iface", envOr("DLNA_IFACE", ""), "network interface for discovery (default: the system's)")
	streams := flag.String("streams", envOr("DLNA_STREAMS", "direct"), "direct: TVs play from the DVR; proxy: through this server")
	keepAds := flag.Bool("keep-ads", envBool("DLNA_KEEP_ADS"), "play recordings with their commercials in, instead of cutting out the breaks the DVR marked")
	state := flag.String("state", envOr("DLNA_STATE", ""), "directory for bookmarks.json (default: keep bookmarks in memory)")
	ffmpeg := flag.String("ffmpeg", envOr("DLNA_FFMPEG", "ffmpeg"), "ffmpeg for the tiny TV's streams")
	tinyMax := flag.Int("tiny-max", envInt("DLNA_TINY_MAX", 2), "tiny TV streams converted at once")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *streams != "direct" && *streams != "proxy" {
		fatal(fmt.Errorf("-streams must be direct or proxy, not %q", *streams))
	}

	client, err := dvr.New(*dvrURL)
	if err != nil {
		fatal(err)
	}
	bookmarks, err := server.OpenBookmarks(*state)
	if err != nil {
		fatal(err)
	}
	_, portStr, err := net.SplitHostPort(*listen)
	if err != nil {
		fatal(fmt.Errorf("-listen: %w", err))
	}
	port, _ := strconv.Atoi(portStr)
	var ifi *net.Interface
	if *iface != "" {
		if ifi, err = net.InterfaceByName(*iface); err != nil {
			fatal(fmt.Errorf("-iface: %w", err))
		}
	}
	uuid := stableUUID(*name, *dvrURL)

	srv := server.New(server.Config{
		Name: *name, UUID: uuid, Version: version,
		DVR: client, DVRPublic: *dvrPublic, Proxy: *streams == "proxy",
		FFmpeg: *ffmpeg, TinyMax: *tinyMax, KeepAds: *keepAds, Bookmarks: bookmarks,
		Logf: log.Printf,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	adv := &ssdp.Advertiser{UUID: uuid, Port: port, Server: server.ServerHeader(version), Host: *advertise, Iface: ifi, Logf: log.Printf}
	go func() {
		if err := adv.Run(ctx); err != nil {
			fatal(err)
		}
	}()

	log.Printf("dlna %s: %q on %s for the DVR at %s, streams %s", version, *name, *listen, *dvrURL, *streams)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the byebye go out
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

func envBool(key string) bool {
	v, _ := strconv.ParseBool(os.Getenv(key))
	return v
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dlna:", err)
	os.Exit(1)
}
