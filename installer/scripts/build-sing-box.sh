#!/usr/bin/env bash
# Builds the four sing-box binaries with the naive outbound (cgo, statically linked against cronet) for the atom
# packages. Run it on the Linux build host (vm92), not on the Mac: cronet needs the Chromium clang toolchain and the
# Debian/OpenWrt sysroots that cronet-go downloads, roughly 3-4 GB per target and architecture.
#
#   installer/scripts/build-sing-box.sh --sing-box-dir DIR --out DIR [--work DIR] [--version STR] [--targets LIST]
#
# DIR is a sing-box checkout at the commit pinned by the atom repository (a plain checkout, git bundle or rsync copy
# is fine). A `go` of 1.25 or newer must be on PATH (vm92's system Go is older: unpack a newer one under --work).
# --work   keeps the cronet-go checkout, the downloaded toolchains and the Go caches (default $HOME/atom-build/naive).
# --targets  any of amd64 arm64 amd64-musl arm64-musl (default: all four). glibc builds link against the Debian
#            bullseye sysroot (glibc 2.31); musl builds use the OpenWrt sysroot and add the with_musl tag.
# Output: DIR/sing-box-<package>  (one per target, named like the atom packages) and DIR/sing-box-commit, which
#         build-package.sh --sing-box-bin reads. Tags and linker flags follow sing-box/.github/workflows/build.yml.
set -euo pipefail

SINGBOX_DIR="" OUT="" WORK="${HOME}/atom-build/naive" VERSION="" TARGETS="amd64 arm64 amd64-musl arm64-musl"
while [ $# -gt 0 ]; do
  case "$1" in
    --sing-box-dir) SINGBOX_DIR="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --work) WORK="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --targets) TARGETS="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -f "$SINGBOX_DIR/release/DEFAULT_BUILD_TAGS" ] || { echo "--sing-box-dir DIR must be a sing-box checkout" >&2; exit 2; }
[ -n "$OUT" ] || { echo "--out DIR is required" >&2; exit 2; }
[ "$(uname -s)" = Linux ] || { echo "run this on the Linux build host" >&2; exit 2; }
for t in $TARGETS; do
  case "$t" in amd64|arm64|amd64-musl|arm64-musl) ;; *) echo "unknown target $t" >&2; exit 2 ;; esac
done
need="$(sed -n 's/^go \([0-9]*\.[0-9]*\).*/\1/p' "$SINGBOX_DIR/go.mod" | head -1)"
have="$(go env GOVERSION | sed 's/^go//')"
[ "$(printf '%s\n%s\n' "$need" "$have" | sort -V | head -1)" = "$need" ] || { echo "go $have is older than the $need sing-box needs" >&2; exit 2; }

SINGBOX_DIR="$(cd "$SINGBOX_DIR" && pwd)"
mkdir -p "$WORK" "$OUT"
WORK="$(cd "$WORK" && pwd)"
OUT="$(cd "$OUT" && pwd)"
export GOPATH="${GOPATH:-$WORK/gopath}" GOFLAGS=-mod=mod
[ -n "$VERSION" ] || VERSION="$(git -C "$SINGBOX_DIR" describe --tags 2>/dev/null | sed 's/^v//' || true)"
[ -n "$VERSION" ] || VERSION=unknown

# cronet-go at the version sing-box pins, without its prebuilt lib/ (the toolchain compiles the library statically)
CRONET="$WORK/cronet-go"
CRONET_VERSION="$(cat "$SINGBOX_DIR/.github/CRONET_GO_VERSION")"
if [ "$(git -C "$CRONET" rev-parse HEAD 2>/dev/null || true)" != "$CRONET_VERSION" ]; then
  rm -rf "$CRONET"
  git init -q "$CRONET"
  git -C "$CRONET" remote add origin https://github.com/sagernet/cronet-go.git
  git -C "$CRONET" sparse-checkout set --no-cone '/*' '!/lib'
  git -C "$CRONET" fetch -q --depth=1 --filter=blob:none origin "$CRONET_VERSION"
  git -C "$CRONET" checkout -q FETCH_HEAD
  git -C "$CRONET" submodule update --init --recursive --depth=1
fi
# the Debian sysroot download verifies packages with this keyring; build.yml regenerates it the same way
if [ ! -f "$WORK/.keyring-ok" ]; then
  command -v gpgv >/dev/null || { echo "gpgv is required (apt-get install gpgv)" >&2; exit 2; }
  rm -f "$CRONET/naiveproxy/src/build/linux/sysroot_scripts/keyring.gpg"
  (cd "$CRONET" && GPG_TTY=/dev/null ./naiveproxy/src/build/linux/sysroot_scripts/generate_keyring.sh)
  touch "$WORK/.keyring-ok"
fi

TAGS_GLIBC="$(cat "$SINGBOX_DIR/release/DEFAULT_BUILD_TAGS")"
LDFLAGS_SHARED="$(cat "$SINGBOX_DIR/release/LDFLAGS")"
for t in $TARGETS; do
  arch="${t%-musl}" libc=""
  tags="$TAGS_GLIBC"
  case "$t" in *-musl) libc="--libc=musl"; tags="$tags,with_musl" ;; esac
  echo "== sing-box $VERSION for linux/$t"
  # shellcheck disable=SC2086
  # downloads from Google and Debian hosts fail now and then; finished parts are kept, so just try again
  ok=""
  for try in 1 2 3 4 5; do
    if (cd "$CRONET" && go run ./cmd/build-naive --target=linux/$arch $libc download-toolchain); then ok=1; break; fi
    echo "download-toolchain failed (attempt $try), retrying" >&2; sleep 10
  done
  [ -n "$ok" ] || { echo "download-toolchain failed for linux/$t" >&2; exit 1; }
  # the env subshell exports CC, CXX and CGO_LDFLAGS for this target only
  (
    cd "$CRONET"
    eval "$(go run ./cmd/build-naive --target=linux/$arch $libc env --export)"
    cd "$SINGBOX_DIR"
    CGO_ENABLED=1 GOOS=linux GOARCH=$arch go build -trimpath -tags "$tags" \
      -ldflags "-X github.com/sagernet/sing-box/constant.Version=$VERSION $LDFLAGS_SHARED -s -w -buildid=" \
      -o "$OUT/sing-box-$t" ./cmd/sing-box
  )
  ls -l "$OUT/sing-box-$t"
done
git -C "$SINGBOX_DIR" rev-parse HEAD > "$OUT/sing-box-commit" 2>/dev/null || echo unknown > "$OUT/sing-box-commit"
