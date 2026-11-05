#!/bin/sh
# Builds the release files into dist/: archives for Linux, macOS and Windows, and
# .deb and .rpm packages for Linux. Needs Go, zip and nfpm (go install
# github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0). The programs are built several
# at a time, JOBS of them (6 unless set).
#   packaging/build.sh 0.99
set -eu
cd "$(dirname "$0")/.."
export CGO_ENABLED=0

# The same commit gives the same files wherever and whenever it is built. File
# times come from the commit; times, order, owners and the locale are fixed.
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}
export SOURCE_DATE_EPOCH TZ=UTC LC_ALL=C

# os arch goarm  nfpm-arch (- for no package)
targets="linux amd64 - amd64
linux arm64 - arm64
linux arm 7 arm7
darwin amd64 - -
darwin arm64 - -
windows amd64 - -
windows arm64 - -"

# name builds the release name of a target: dlna-bridge_1.2_linux_armv7
name() { # os arch arm
  if [ "$3" != "-" ]; then echo "dlna-bridge_${VERSION}_$1_armv$3"; else echo "dlna-bridge_${VERSION}_$1_$2"; fi
}

# One target: the program and its archive. Run several at once by the part
# below, so it touches only its own directory.
if [ "${1:-}" = "--one" ]; then
  VERSION=$2 os=$3 arch=$4 arm=$5
  n=$(name "$os" "$arch" "$arm")
  bin=build/$n/dlna-bridge
  [ "$os" = windows ] && bin=$bin.exe
  mkdir -p "build/$n"
  GOOS=$os GOARCH=$arch GOARM=${arm#-} go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$bin" ./cmd/dlna
  cp README.md "build/$n/"
  find "build/$n" -exec touch -d "@$SOURCE_DATE_EPOCH" {} +
  if [ "$os" = windows ]; then
    (cd build && find "$n" | sort | zip -X -q "../dist/$n.zip" -@)
  else
    tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner -C build -cf - "$n" | gzip -n > "dist/$n.tar.gz"
  fi
  exit 0
fi

VERSION=${1:?usage: packaging/build.sh VERSION}
VERSION=${VERSION#v}
JOBS=${JOBS:-6}
rm -rf dist build && mkdir -p dist build

echo "$targets" | cut -d' ' -f1-3 | xargs -P "$JOBS" -L1 "$0" --one "$VERSION"

# The packages one after another: they are quick, and nfpm does not expand
# variables in file names, so each binary goes where the config says.
echo "$targets" | while read -r os arch arm pkg; do
  [ "$pkg" = "-" ] && continue
  bin=build/$(name "$os" "$arch" "$arm")/dlna-bridge
  cp "$bin" build/dlna-bridge-pkgbin
  touch -d "@$SOURCE_DATE_EPOCH" build/dlna-bridge-pkgbin packaging/dlna-bridge.service packaging/dlna-bridge.env packaging/postinstall.sh packaging/preremove.sh
  for fmt in deb rpm; do
    VERSION=$VERSION ARCH=$pkg \
      nfpm package --config packaging/nfpm.yaml --packager $fmt --target dist/
  done
done
(cd dist && sha256sum * > SHA256SUMS)
ls -l dist
