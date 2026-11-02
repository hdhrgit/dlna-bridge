# dlna

A DLNA media server for a Channels DVR, so TVs without a Channels app (LG, Samsung) can play its live channels
and recordings, and a tiny 240x240 ESP32 board can play the same channels.

```
 LG / Samsung TV ──browse──► dlna (:8200) ──listing──► Channels DVR (:8089)
        └──────────────── plays the DVR's own stream ───────┘
 tiny TV ──► dlna /tiny/<number>.ts ──ffmpeg──◄── DVR's small HLS stream
```

The big TVs only browse the bridge; they play `/devices/ANY/channels/<number>/stream.mpg` and
`/dvr/files/<id>/stream.mpg` straight from the DVR, at full quality and with no conversion. The tiny TV gets one
ffmpeg per viewer, because the DVR's transcoder makes H.264 Main profile with B-frames and the board decodes
baseline only.

## Run

```sh
docker compose up -d --build
```

The container uses host networking: discovery is UDP multicast on the LAN, which a bridged container never sees.
With the DVR on the same machine, `DLNA_DVR=http://localhost:8089` works and the TVs are sent to this machine's
LAN address on the DVR's port. Open `http://HOST:8200/` to check the bridge reaches the DVR.

| Flag | Env | Default | |
|---|---|---|---|
| `-listen` | `DLNA_LISTEN` | `:8200` | HTTP address |
| `-dvr` | `DLNA_DVR` | `http://localhost:8089` | the DVR, as the bridge reaches it |
| `-dvr-public` | `DLNA_DVR_PUBLIC` | | the DVR as the TVs reach it, when `-dvr` is not that |
| `-name` | `DLNA_NAME` | `Channels DVR` | the name the TVs list |
| `-streams` | `DLNA_STREAMS` | `direct` | `proxy` sends the TVs' streams through the bridge, which adds the DLNA headers some TVs want |
| `-advertise` | `DLNA_ADVERTISE` | | the address to announce, when the machine has several |
| `-iface` | `DLNA_IFACE` | | the interface for discovery |
| `-keep-ads` | `DLNA_KEEP_ADS` | `false` | play recordings with their commercials in |
| `-state` | `DLNA_STATE` | `/var/lib/dlna` in Docker | where Samsung resume points are kept |
| `-ffmpeg` | `DLNA_FFMPEG` | `ffmpeg` | |
| `-tiny-max` | `DLNA_TINY_MAX` | `2` | tiny TV streams converted at once; more get 503 |

The server's id comes from the host name, `-name` and `-dvr`, so it stays the same across restarts and TVs do
not list it twice.

## What the TVs see

* **Live TV**: every channel that is not hidden in the Channels apps, in number order, titled `<number> <name> - <show on now>` from the DVR's
  `/devices/ANY/guide/now`, with its logo. Live streams cannot seek.
* **Favorites**: the channels starred in the Channels apps, with the same titles. The folder is left out when
  there are none.
* **Recordings**: a folder per show (the DVR's groups), newest first, episodes titled `S01E02 Title`. They
  take byte ranges, so the TVs can seek. Samsung TVs save where they stopped (`X_SetBookmark`) and offer to
  resume; before that, they resume where the Channels apps left off.

## Commercials

DLNA has no way to tell a TV to skip a stretch, so the bridge leaves the breaks out. Once the DVR has finished a
recording and marked its breaks (`Commercials` in `/dvr/files/<id>`, the same marks as `comskip.edl`), the TVs
play it from `/stream/adfree/<id>`: the DVR's file with the bytes of each break left out, nothing re-encoded.
The bridge finds each break's start by bisecting the file on video timestamps over byte ranges, starts the next
part on the first keyframe after the break, and puts the PAT and PMT in front of it. The result is one file of
whole packets with a length, so the TVs can still seek; its listed duration and resume point leave the breaks
out. The timestamps jump at each join, as they do after a broadcast splice.

Working out the cuts takes a few dozen range reads and is kept per recording. If it fails, the whole recording
is served. `-keep-ads` (`DLNA_KEEP_ADS=true`) turns this off and points the TVs at the DVR's file again.

## Tiny TV

`/tiny/<number>.ts` is a live MPEG-TS with no length and no seeking: H.264 Constrained Baseline, no B-frames,
24 fps, 240 wide (240x134 for 16:9, 240x180 for 4:3), about 250 kbps; AAC-LC mono 44.1 kHz at 64 kbps. The
bridge asks the DVR for its 180-line, 600 kbps HLS stream and converts that. `/tiny/channels.m3u` lists every
channel's address. The board needs a live MPEG-TS input before it can play these; today it reads AVI files.

## Develop

No Go is needed on the build machine; the Dockerfile builds it. With Go 1.25 or newer:

```sh
go test ./...
go run ./cmd/dlna -dvr http://dvr-host:8089
```
