# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.99.3
WORKDIR /src
COPY go.mod ./
COPY . .
# Exactly the Go in .go-version, as the release files are built with.
RUN export GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 GOTOOLCHAIN="go$(cat .go-version)" \
 && go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/dlna ./cmd/dlna

# ffmpeg converts streams for clients set to that.
FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/dlna /usr/local/bin/
# bookmarks.json lives here; map a volume or Samsung resume points are lost with the container.
WORKDIR /var/lib/dlna
VOLUME /var/lib/dlna
EXPOSE 8200 1900/udp
ENTRYPOINT ["/usr/local/bin/dlna"]
CMD ["-state", "/var/lib/dlna"]
