#!/bin/sh
# Prints the version to build: the tag on this commit (v1.2.3 gives 1.2.3), or the
# last tag and how far past it, as 1.2.3+4.gabc1234 (a build metadata suffix, so
# it sorts after the release it follows). Without any tag: 0.0.0+gabc1234.
set -eu
cd "$(dirname "$0")/.."
git config --global --add safe.directory "$PWD" 2>/dev/null || true
d=$(git describe --tags --always --long 2>/dev/null || echo "g$(git rev-parse --short=7 HEAD)")
case $d in
  v*-*-g*)
    tag=${d%-*-g*}; rest=${d#"$tag"-}; n=${rest%%-g*}; sha=g${rest#*-g}
    if [ "$n" = 0 ]; then echo "${tag#v}"; else echo "${tag#v}+$n.$sha"; fi ;;
  *) echo "0.0.0+$d" ;;
esac
