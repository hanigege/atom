#!/usr/bin/env bash
# DEVELOPMENT ONLY. Until the wizard of the container version exists (docs/container.md, C2), this lays down what
# the wizard would: a data directory for the container's volume, in the state of a finished single-NIC installation.
#
#   dev-container-seed.sh --package FILE|DIR --data DIR --lan IFACE [--port 80] [--backend sing-box|mihomo]
#
# --package  a package tar.gz of build-package.sh, or the unpacked directory (atom-<version>/)
# --data     the directory that is mounted at /opt/atom/data (created; must be new or empty)
# --lan      the network interface the container will have (the one macvlan gives it, normally eth0)
set -euo pipefail
PACKAGE="" DATA="" LAN="" PORT=80 BACKEND=sing-box
while [ $# -gt 0 ]; do
  case "$1" in
    --package) PACKAGE="$2"; shift 2 ;;
    --data) DATA="$2"; shift 2 ;;
    --lan) LAN="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --backend) BACKEND="$2"; shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$PACKAGE" ] && [ -n "$DATA" ] && [ -n "$LAN" ] || { echo "--package, --data and --lan are required" >&2; exit 2; }
[ "$BACKEND" = sing-box ] || [ "$BACKEND" = mihomo ] || { echo "--backend is sing-box or mihomo" >&2; exit 2; }
[ -z "$(ls -A "$DATA" 2>/dev/null)" ] || { echo "$DATA is not empty" >&2; exit 2; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
if [ -d "$PACKAGE" ]; then PKG="$PACKAGE"; else tar -xzf "$PACKAGE" -C "$TMP"; PKG="$(echo "$TMP"/atom-*)"; fi
VERSION="$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$PKG/manifest.json" | head -1)"
INSTALLER=/opt/atom/bin/atom-installer
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

mkdir -p "$DATA" "$DATA/.atom" "$DATA/ui/main" "$DATA/webinfo"
cp -R "$PKG/config/." "$DATA/"
tar -xzf "$PKG/atom-webui.tar.gz" -C "$DATA/ui/main"
# the panel port, and the reload and stop commands of the kernels (what setReloadCommand writes for a container)
sed -i.bak -E "/^api:/,/^[^ ]/ s|(http: *\")[^\"]*(\")|\10.0.0.0:$PORT\2|" "$DATA/config_custom.yaml"
for k in sing-box mihomo; do
  sed -i.bak -E \
    -e "s|\[systemctl, reload-or-restart, atom-$k.service\]|[$INSTALLER, svc, reload, atom-$k]|" \
    -e "s|\[systemctl, stop, atom-$k.service\]|[$INSTALLER, svc, stop, atom-$k]|" "$DATA/sub_config/atom_rules.yaml"
done
rm -f "$DATA/config_custom.yaml.bak" "$DATA/sub_config/atom_rules.yaml.bak"
grep -q "http: \"0.0.0.0:$PORT\"" "$DATA/config_custom.yaml" || { echo "could not set the panel port" >&2; exit 1; }
! grep -q systemctl "$DATA/sub_config/atom_rules.yaml" || { echo "atom_rules.yaml still calls systemctl" >&2; exit 1; }
for k in sing-box mihomo; do
  for a in reload stop; do
    [ "$(grep -c -E "^ +$a: \\[$INSTALLER, svc, $a, atom-$k\\]\$" "$DATA/sub_config/atom_rules.yaml")" = 1 ] || { echo "atom_rules.yaml has not got exactly one '$a' command for $k" >&2; exit 1; }
  done
done

cat > "$DATA/webinfo/atom_proxy.json" <<JSON
{
  "version": 0,
  "nodes": [],
  "groups": [{"id": "g-default", "name": "默认代理", "type": "selector", "members": []}],
  "final": {"members": ["g-default"], "selected": "g-default"},
  "settings": {"backend": "$BACKEND", "fakeip_range": "198.18.0.0/15", "mixed_port": 7890}
}
JSON
chmod 600 "$DATA/webinfo/atom_proxy.json"
cat > "$DATA/.atom/install.json" <<JSON
{
  "version": "$VERSION",
  "complete": true,
  "installed_at": "$NOW",
  "updated_at": "$NOW",
  "topology": "bypass",
  "lan": ["$LAN"],
  "panel_port": $PORT,
  "nft_bin": "/usr/sbin/nft",
  "sysctl_bin": "/usr/sbin/sysctl"
}
JSON
echo "{\"version\": \"$VERSION\"}" > "$DATA/.atom-data"
echo "seeded $DATA for atom $VERSION (LAN $LAN, panel port $PORT, kernel $BACKEND)"
