# dlna

A DLNA media server for a Channels DVR, so TVs without a Channels app (LG, Samsung) can play its live channels
and recordings.

```
 TV ──browse and play──► dlna (:8200) ──listing and streams──► Channels DVR (:8089)
```

By default the TVs browse the bridge and play through it: it passes the DVR's own stream on, at full quality,
with the headers TVs expect. That works for every TV, including those that insist on a `HEAD` request, which the DVR
refuses. A client can be set to take the DVR's stream *directly* instead, which spares the bridge the traffic, but
it is an optimization that not every TV accepts; or to have it converted. Each kind of client can be set up
differently on the settings page: see [Clients](#clients).

## Run

### Packages

Each release has `.deb` and `.rpm` packages for linux amd64, arm64 and armv7, and plain archives for Linux, macOS
and Windows, on the [releases page](https://github.com/hdhrgit/dlna-bridge/releases). They hold the program, a
systemd service and a settings file, and not ffmpeg: that is only needed by clients set to *Converted*, and is
suggested, not required.

```sh
sudo apt install ./dlna-bridge_0.99.11_amd64.deb     # or: sudo dnf install ./dlna-bridge-0.99.11-1.x86_64.rpm
sudoedit /etc/dlna-bridge/dlna-bridge.env             # set DLNA_DVR to the Channels DVR
sudo systemctl enable --now dlna-bridge
```

The service keeps `config.json` in `/var/lib/dlna-bridge`. The macOS and Windows archives are built but not
tested; run the program with `-dvr http://DVR:8089` and the flags below.

### Docker

A multi-platform image (linux/amd64, linux/arm64 and linux/arm/v7) is published for each release, with ffmpeg in it:

```sh
docker run -d --name dlna-bridge --restart unless-stopped --init --network host \
  -v dlna-state:/var/lib/dlna \
  -e DLNA_DVR=http://localhost:8089 \
  ghcr.io/hdhrgit/dlna-bridge:0.99.11
```

or save `docker-compose.yml` from this repo, set `DLNA_DVR` in it, and run `docker compose up -d`. To build from
the source instead: `docker compose up -d --build`. Set `DLNA_DVR` to the address of the Channels DVR as this
machine reaches it (`localhost` when it runs here). Host networking needs Docker on Linux; Docker Desktop for
Mac and Windows does not pass multicast through.

The container uses host networking: discovery is UDP multicast on the LAN, which a bridged container never sees.
With the DVR on the same machine, `DLNA_DVR=http://localhost:8089` works and the TVs are sent to this machine's
LAN address on the DVR's port. Open `http://HOST:8200/` to check the bridge reaches the DVR.

| Flag | Env | Default | |
|---|---|---|---|
| `-listen` | `DLNA_LISTEN` | `:8200` | HTTP address; the settings page is at `/` |
| `-dvr` | `DLNA_DVR` | `http://localhost:8089` | the DVR, as the bridge reaches it |
| `-dvr-public` | `DLNA_DVR_PUBLIC` | | the DVR as the TVs reach it, when `-dvr` is not that |
| `-name` | `DLNA_NAME` | `Channels DVR` | the name the TVs list |
| `-state` | `DLNA_STATE` | `/var/lib/dlna` in Docker | where `config.json` and the Samsung resume points are kept |
| `-config` | `DLNA_CONFIG` | `config.json` in `-state` | the file the settings page writes |
| `-dvr-max-stale` | `DLNA_DVR_MAX_STALE` | `15m` | how old the channel and recording lists may be, when the DVR does not answer, and still be served; after that the DVR's error is shown |
| `-user-agent` | `DLNA_USER_AGENT` | `dlna-bridge/<version>` | what the bridge calls itself to the DVR, for its lists, streams and ffmpeg |
| `-client-name` | `DLNA_CLIENT_NAME` | `DLNA Bridge` | what the DVR calls the bridge in its client list and activity list, apart from the DLNA name the TVs see; empty: do not register |
| `-advertise` | `DLNA_ADVERTISE` | | the address to announce, when the machine has several |
| `-advertise-port` | `DLNA_ADVERTISE_PORT` | the port of `-listen` | the port to announce, when the TVs reach the server through a proxy on another one |
| `-trusted-proxies` | `DLNA_TRUSTED_PROXIES` | | addresses or ranges of the proxies in front of the server, whose `X-Forwarded-For` and `X-Forwarded-Host` are believed |
| `-iface` | `DLNA_IFACE` | | the interface for discovery |
| `-plan-ahead` | `DLNA_PLAN_AHEAD` | `true` | work out the cuts of each new recording once the DVR has marked its breaks (see [Commercials](#commercials)) |
| `-plan-ahead-breaks` | `DLNA_PLAN_AHEAD_BREAKS` | `6` | plan ahead only recordings with at least this many breaks; `1` plans every one, `0` none |
| `-ffmpeg` | `DLNA_FFMPEG` | `ffmpeg` | |
| `-streams`, `-keep-ads`, `-max-transcodes` | `DLNA_STREAMS`, `DLNA_KEEP_ADS`, `DLNA_MAX_TRANSCODES` | | the first-run default profile, until the settings page saves a file |

The server's id comes from the host name, `-name` and `-dvr`, so it stays the same across restarts and TVs do
not list it twice.

## Behind a reverse proxy

TVs find the server by multicast, which stays on the machine the bridge runs on, but they can be sent to a
proxy in front of it: announce the proxy's port with `-advertise-port` (and the address with `-advertise` if the
bridge's machine is not the proxy's), and list the proxy in `-trusted-proxies` so that the bridge still sees each
TV's own address, which clients (see below) match on. Without that, every request would seem to come from the
proxy.

```nginx
server {
    listen 8280;
    location / {
        proxy_pass http://127.0.0.1:8200;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $http_host;                          # the links the bridge hands out are on this host
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Host $http_host;
        proxy_buffering off;                                       # streams must not wait in a buffer
        proxy_request_buffering off;
        proxy_read_timeout 1h;                                     # waiting for the bridge: a stalled channel, or a TV paused with a full buffer
        send_timeout 1h;                                           # sending to the TV: it may stay paused for a long time
    }
}
```

```sh
dlna-bridge -advertise-port 8280 -trusted-proxies 127.0.0.1      # or DLNA_ADVERTISE_PORT and DLNA_TRUSTED_PROXIES
```

The TVs speak plain http, so the proxy should not redirect to https. Streams that a client takes straight from the
DVR (*Direct*) do not pass through the proxy; use *Through the bridge* for those that should.

## How the DVR sees the bridge

The DVR names a viewer by the address it connects from. Without a name, everything the bridge sends, for any TV,
shows in the DVR's activity list as "Watching … from 192.168.1.10", the bridge's address. At start-up, and every
half hour after, the bridge registers with the DVR (`POST /dvr/clients/register`) under `-client-name`, so the
activity list says "from DLNA Bridge", and the DVR's client list has an entry of that name. The name is separate
from the DLNA name (`-name`) that the TVs show. The status page says how it went, and under *What the DVR says
it is doing* shows only what the DVR does for the bridge's address.

The DVR tells clients apart by address alone. If another client reaches the DVR from the same address as the
bridge, for example both through a proxy on the DVR's machine, so both come from `127.0.0.1`, the DVR takes them
for one client, under one name, and the bridge's streams may show under the other's. The status page and the
log say so. Point `-dvr` at an address of the DVR that only the bridge uses, such as its LAN address and own
port (`http://192.168.1.10:8089`) instead of the proxy. All the TVs share the one
name, since they all reach the DVR through the bridge. A client set to *Direct* is not the bridge's: the DVR
names it by that TV's own address.

## What the TVs see

* **Live TV**: every channel that is not hidden in the Channels apps, in number order, titled
  `<number> <name> - <show on now>` from the DVR's `/devices/ANY/guide/now`, with its logo. Live streams cannot seek.
* **Favorites**: the channels starred in the Channels apps. Left out when there are none.
* **Channels by source**: a folder for each source (tuner, M3U list...), its channels played from that source.
* **Recent recordings**: what started in the last 24 hours (the page sets how many), newest first, each titled
  with its show, since a TV has no other way to see what is new. Left out when there are none.
* **TV Shows**: a folder per show (the DVR's groups), newest first, episodes titled `S01E02 Title`. They
  take byte ranges, so the TVs can seek. Samsung TVs save where they stopped (`X_SetBookmark`) and offer to
  resume; before that, they resume where the Channels apps left off.
* **Movies**: the recorded movies (the DVR's Movies group), newest first, each by its title. They play and
  seek like the shows. Left out when there are none.
* **Search**: TVs that search (UPnP Search) find channels, episodes and movies by title, class, description
  or channel. Episodes come back titled with their show.
* **Audio** and **Audio by source**: the live channels as sound alone, for players without a screen, in the
  client's audio format: AAC or MP3 at the client's bitrate, or the channel's own sound unconverted (MPEG-TS).
  The DVR sends the sound alone (its HLS with `vcodec=none&acodec=copy`), a few seconds behind its full stream;
  ffmpeg converts it, counted among the conversions at once.
* **Recording now**: what the DVR has not finished recording, from the start. Left out when nothing is.

TVs keep the lists they have browsed. The bridge follows the DVR's own event stream (`/dvr/events/subscribe`),
and when a recording starts, finishes, has its breaks marked or is deleted, it tells the TVs that subscribed to
its change events, so that they browse again. If the stream drops, as when the DVR restarts, the bridge looks at
the DVR's newest recordings every minute until it connects again, and catches up on what changed meanwhile. The
channel folders are titled with the show on now, so the bridge also looks at the guide every 5 minutes and,
when a show has changed, tells the TVs that Live TV, Favorites and Channels by source changed. A TV that ignores the events shows the new recording when its folder
is opened again, or after its media app is restarted.

## Clients

Open `http://HOST:8200/`. Under *Clients that have connected* are the devices that have used the bridge, with
the User-Agent each sent; *Make a client* starts a profile from one. The page writes `config.json`; changes
apply the next time a TV browses.

A **client** is a name, a list of addresses (`10.0.0.7` or `192.168.1.0/24`) and a list of User-Agent texts. A
request fits when its address is listed or its User-Agent contains one of the texts. The first client that fits
decides; anything else gets the default. A stream URL can also pick a client by name with `?profile=NAME`, for
a device that has no fixed address. `/channels.m3u?profile=NAME` lists every channel that way, for players that
take a playlist.

One client is **built in**: `esp32`, for the [channelstv](https://github.com/hdhrgit/channelstv) boards, used
only by name (`?profile=esp32`). It converts to H.264 Constrained Baseline with one slice per frame and no
deblocking filter (less work for the boards' decoder), 240x144 stretched, 24 fps, 250 kbit/s video, AAC mono
64 kbit/s, from the DVR's small stream for live channels, with commercials cut and seeking by time; each board
asks for its own size with the overrides below. It needs no
setting up. The settings page shows it read-only, with *Edit a copy*: a client of the same name in `config.json`
takes its place, and removing that client brings the built-in one back.

Each client and the default have a **profile**:

| | |
|---|---|
| Shows | **Video and audio** (the default), **Video only** (no Audio folders), or **Audio only** (just the Audio folders, for a player without a screen). Folders a client is not shown are not found by id or search either. |
| Streams | **Through the bridge** (the default): the DVR's stream passed through, with the DLNA headers TVs expect. **Direct**: the client plays the DVR's own stream, which the bridge never sees: an optimization for TVs that accept it (the LGs do; a Samsung does not). **Converted**: ffmpeg converts it, below. |
| Commercials | **Cut out**, or **Play** (pass through). |
| Resume and watched | **Shared with the DVR** (the default): where the client leaves a recording goes to the DVR, and one played to the end is marked watched, so the Channels apps resume there too. **Kept here**: the DVR is not told. |
| Seeking | **By bytes** (the default): a recording with its breaks cut has a length and takes byte ranges, which every TV knows; the bridge works out all the cuts before the first byte. A converted recording has no length and does not seek. **By time**: cut and converted recordings are offered with DLNA time seek (`DLNA.ORG_OP=10`) and no length, and the TV asks for a time to start from (`TimeSeekRange.dlna.org`). A cut one starts at once, each cut worked out just before playback reaches it; a converted one starts with ffmpeg seeking the input by byte ranges (with commercials cut, the seconds leave the breaks out). The answer carries the range served; a time seek from a client set to seek by bytes gets 406. Not every TV seeks by time; LG TVs do not. |
| Converted | The largest width and height (the picture is fitted inside, both sides even), frames per second, video and audio kbps, audio channels, H.264 profile (baseline has no B-frames and no CABAC, for small decoders). Live channels can come from a small stream the DVR makes first, which costs it less than decoding the full picture here, but the DVR's small stream runs about 10 s behind its full one; leave it off where delay matters. A live channel is kept near its live edge: a client that falls more than 8 s behind skips ahead to the newest keyframe. The result is an MPEG-TS with no length, so it does not seek by bytes. |

At most *Conversions at once* run, across all clients; the rest get a 503. Each viewer has an ffmpeg of their
own, which stops when they leave.

A converted stream (`/stream/live/N`, `/stream/rec/ID`, `/stream/adfree/ID`) can change its profile's picture
and sound for one request, e.g. `?profile=NAME&w=320&h=180&fit=scale&fps=24&vb=400&ab=64`: `w` and `h` the
size (rounded down to even), `fit` one of `scale` (stretched to that size), `crop43` (the middle 4:3, then
stretched), `width` (the whole picture, as wide as that size allows, e.g. 448x252 from 16:9 at 448x368),
`height` (as tall as that size, the sides cut evenly) or `fill` (the whole size covered, what is over cut
evenly), `fps` the most frames per second, `vb` and `ab` the video and audio kbit/s. The rest (codec,
H.264 profile, slices, small stream, commercials, audio channels) stays the profile's. Values beyond the limits
under *Other settings* (by default 640x480, 30 fps, 1500 and 128 kbit/s) are held to them; values that are
not numbers are ignored, as are overrides on a stream that is not converted. A live channel from the DVR's
small stream gets at most 64 kbit/s audio, which is all that stream has. `GET /api/caps` lists the overrides,
the fits, the limits and the built-in profiles (`"profiles": ["esp32"]`), and each converted stream carries
what it was made with, e.g. `X-Transcode: 320x180 fit=scale fps=24 vb=400 ab=64`, with the size and frame rate
actually sent. A `HEAD`, which reads nothing, gives the most where the source decides them.

## Commercials

DLNA has no way to tell a TV to skip a stretch, so the bridge leaves the breaks out. Once the DVR has finished a
recording and marked its breaks (`Commercials` in `/dvr/files/<id>`, the same marks as `comskip.edl`), clients
set to cut them play it from `/stream/adfree/<id>`: the DVR's file with the bytes of each break left out,
nothing re-encoded. The bridge finds each break's start by bisecting the file on video timestamps over byte
ranges, starts the next part on the first keyframe after the break, and puts the PAT and PMT in front of it. The
timestamps and clock marks after a break are moved back by the length of the breaks before it, so time runs on
without a jump. The pictures right after the keyframe that are shown before it refer to the cut footage and
would decode as garbage, so their packets are replaced by empty ones.

The result is one file of whole packets with a length, so the TVs can still seek; its listed duration and resume
point leave the breaks out. Working out the cuts takes a few hundred range reads of the recording (a second or
two for a movie with 17 breaks): the search for each mark starts from the frames found for the earlier ones and
estimates where the time is from the bitrate, instead of halving the file. The cut list is kept in memory for the
32 recordings played last and in `plans` in the state directory, so a restart does not work it out again; it is
replaced when the recording's size or marked breaks change.

So that the first play of a new recording does not wait for that, the bridge works out its cuts as soon as the
DVR has marked its breaks. Only new recordings: every 2 minutes it asks the DVR for its 25 newest
(`/api/v1/all?sort=date_added&order=desc&limit=25`, a few kilobytes, not the whole library) and plans those that
were finished and marked since the bridge started and have 6 breaks or more (`-plan-ahead-breaks`), one at a time, each a few hundred
range reads. One with fewer breaks is planned in well under a second on first play, and planning ahead a
recording that is never watched would cost the DVR the reads for nothing. The library
already there is planned on first play, as before. It needs the state directory, and does nothing while no
client has its commercials cut. `-plan-ahead-breaks 0` turns it off.

A recording can hold damaged packets, cut short where the signal dropped, after which the packets are no longer
every 188 bytes from the start. The bridge finds the packets again after each one, and passes the damaged bytes on
as they are, as the DVR's own stream would.

A client set to seek *by time* does not wait for the cut list: the bridge finds the first keyframe after the
time asked for, sends from there, and finds each later break as the stream nears it. If a break cannot be found
then, the rest of the recording is sent uncut.

If the recording's video timestamps jump (a splice, an encoder restart), they no longer keep time with the DVR's
marks, and cutting by them would remove the wrong stretches. The bridge notices, logs it once, and serves that
recording whole.

## Develop

The release files are built with exactly the Go in `.go-version`, on any machine, which is what makes the same
commit give the same files everywhere. To move to a newer Go, change that one file.

No Go is needed on the build machine; the Dockerfile builds it. With Go 1.25 or newer:

```sh
go test ./...
go run ./cmd/dlna -dvr http://dvr-host:8089
```
