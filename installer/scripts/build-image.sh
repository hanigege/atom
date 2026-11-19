#!/usr/bin/env bash
# Builds the atom container image from a package that build-package.sh made, and saves it as a docker-archive tar.
# Nothing is compiled: the programs in the image are the ones in the package (docs/container.md section 4).
#
#   installer/scripts/build-image.sh --version 0.3.0 --package DIR|FILE [--arch amd64] [--base IMAGE] \
#       [--installer FILE] [--tag NAME] [--out DIR] [--docker CMD]
#
# --package   the directory build-package.sh wrote (atom-<version>-linux-<arch>.tar.gz, the glibc one, is taken from
#             it and checked against SHA256SUMS when that is there), or one such tar.gz
# --arch      amd64 (arm64 comes with C7)
# --base      the base image instead of debian:trixie-slim, for a host that cannot reach Docker Hub and the Debian
#             mirror: it must already have nftables, iproute2, ca-certificates and tzdata (apt is not run then)
# --installer replace the atom-installer of the package (development: a newly built one)
# --tag       image name (default atom:<version>)
# --out       where the tar goes (default installer/dist): atom-<version>-container-<arch>.tar
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTEXT_SRC="$(cd "$HERE/../container" && pwd)"
VERSION="" PACKAGE="" ARCH=amd64 BASE="" INSTALLER_BIN="" TAG="" OUT="$HERE/../dist" DOCKER=docker

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="$2"; shift 2 ;;
    --package) PACKAGE="$2"; shift 2 ;;
    --arch) ARCH="$2"; shift 2 ;;
    --base) BASE="$2"; shift 2 ;;
    --installer) INSTALLER_BIN="$2"; shift 2 ;;
    --tag) TAG="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --docker) DOCKER="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "--version X.Y.Z is required" >&2; exit 2; }
[ "$ARCH" = amd64 ] || { echo "only --arch amd64 for now" >&2; exit 2; }
[ -n "$PACKAGE" ] || { echo "--package DIR|FILE is required" >&2; exit 2; }
[ -z "$INSTALLER_BIN" ] || [ -f "$INSTALLER_BIN" ] || { echo "no such file: $INSTALLER_BIN" >&2; exit 2; }
[ -n "$TAG" ] || TAG="atom:$VERSION"

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

ARCHIVE="$PACKAGE"
if [ -d "$PACKAGE" ]; then
  ARCHIVE="$PACKAGE/atom-$VERSION-linux-$ARCH.tar.gz"
  [ -f "$ARCHIVE" ] || { echo "missing $ARCHIVE" >&2; exit 2; }
  if [ -f "$PACKAGE/SHA256SUMS" ]; then
    want="$(awk -v f="$(basename "$ARCHIVE")" '$2 == f { print $1 }' "$PACKAGE/SHA256SUMS")"
    [ -n "$want" ] && [ "$want" = "$(sha256 "$ARCHIVE")" ] || { echo "$ARCHIVE does not match SHA256SUMS" >&2; exit 2; }
  fi
fi
[ -f "$ARCHIVE" ] || { echo "missing $ARCHIVE" >&2; exit 2; }

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
tar -xzf "$ARCHIVE" -C "$STAGE"
PKG="$STAGE/atom-$VERSION"
[ -f "$PKG/manifest.json" ] || { echo "$ARCHIVE is not the package of $VERSION" >&2; exit 2; }
grep -q '"arch": "'"$ARCH"'"' "$PKG/manifest.json" || { echo "$ARCHIVE is not an $ARCH package" >&2; exit 2; }
grep -q '"libc": "glibc"' "$PKG/manifest.json" || { echo "$ARCHIVE is not a glibc package (the image is Debian)" >&2; exit 2; }
[ -z "$INSTALLER_BIN" ] || cp "$INSTALLER_BIN" "$PKG/bin/atom-installer"

# the build context: /opt/atom/bin (read-only programs) and /opt/atom/share (what a first start and an upgrade copy from)
CTX="$STAGE/ctx"
mkdir -p "$CTX/atom/bin" "$CTX/atom/share"
for b in atomdns sing-box mihomo atom-installer; do
  [ -f "$PKG/bin/$b" ] || { echo "the package has no bin/$b" >&2; exit 2; }
  install -m 0755 "$PKG/bin/$b" "$CTX/atom/bin/$b"
done
cp -R "$PKG/config" "$CTX/atom/share/config"
cp "$PKG/atom-webui.tar.gz" "$PKG/manifest.json" "$CTX/atom/share/"
cp "$CONTEXT_SRC/Dockerfile" "$CTX/Dockerfile"

args=(--platform "linux/$ARCH" --tag "$TAG" --label "org.opencontainers.image.version=$VERSION")
[ -z "$BASE" ] || args+=(--build-arg "BASE=$BASE" --build-arg DEPS=skip)
$DOCKER build "${args[@]}" "$CTX"

mkdir -p "$OUT"
TAR="$OUT/atom-$VERSION-container-$ARCH.tar"
rm -f "$TAR"
$DOCKER save -o "$TAR" "$TAG"
echo "built image $TAG, wrote $TAR ($(du -h "$TAR" | cut -f1), sha256 $(sha256 "$TAR"))"
