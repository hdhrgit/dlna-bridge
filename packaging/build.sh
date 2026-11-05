#!/bin/sh
# Builds the release files into dist/: archives for Linux, macOS and Windows, and
# .deb and .rpm packages for Linux. Needs Go and nfpm (go install
# github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.48.0).
#   packaging/build.sh 0.99
set -eu
VERSION=${1:?usage: packaging/build.sh VERSION}
VERSION=${VERSION#v}
cd "$(dirname "$0")/.."
rm -rf dist && mkdir -p dist build
export CGO_ENABLED=0
ldflags="-s -w -X main.version=$VERSION"

# os arch goarm  nfpm-arch (empty for no package)
targets="linux amd64 - amd64
linux arm64 - arm64
linux arm 7 arm7
darwin amd64 - -
darwin arm64 - -
windows amd64 - -
windows arm64 - -"

echo "$targets" | while read -r os arch arm pkg; do
  name=dlna-bridge_${VERSION}_${os}_${arch}
  [ "$arm" != "-" ] && name=dlna-bridge_${VERSION}_${os}_armv${arm}
  bin=build/$name/dlna-bridge
  [ "$os" = windows ] && bin=$bin.exe
  mkdir -p "build/$name"
  GOOS=$os GOARCH=$arch GOARM=${arm#-} go build -trimpath -ldflags "$ldflags" -o "$bin" ./cmd/dlna
  cp README.md "build/$name/"
  if [ "$os" = windows ]; then
    (cd build && zip -qr "../dist/$name.zip" "$name")
  else
    tar -C build -czf "dist/$name.tar.gz" "$name"
  fi
  if [ "$pkg" != "-" ]; then
    # nfpm does not expand variables in file names, so the binary goes where the
    # config says.
    cp "$bin" build/dlna-bridge-pkgbin
    for fmt in deb rpm; do
      VERSION=$VERSION ARCH=$pkg \
        nfpm package --config packaging/nfpm.yaml --packager $fmt --target dist/
    done
  fi
done
(cd dist && sha256sum * > SHA256SUMS)
ls -l dist
