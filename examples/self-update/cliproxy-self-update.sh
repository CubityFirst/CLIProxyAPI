#!/bin/bash
# Installs a CLIProxyAPI release requested from the Management Center.
#
# Runs as root from cliproxy-self-update.service when the server writes
# $UPDATE_DIR/request.json (POST /v8/management/server/update). The request only
# names a tag; every trust decision is made here:
#   - the repository comes from this host's config, never from the request;
#   - the archive must match the SHA-256 digest GitHub publishes for the asset;
#   - the extracted binary must report the requested version;
#   - a restarted server that isn't healthy is rolled back to the previous binary.
# Progress is written to $UPDATE_DIR/status.json for the panel. Files in the
# shared directory are only ever read and written as the service user.
set -euo pipefail

CONF=${CLIPROXY_SELF_UPDATE_CONF:-/etc/cliproxy-self-update.env}
# shellcheck source=/dev/null
[ -r "$CONF" ] && . "$CONF"
: "${REPO:?set REPO (owner/repo) in $CONF}"
: "${UPDATE_DIR:?set UPDATE_DIR in $CONF}"
SERVICE=${SERVICE:-cliproxyapi}
BIN=${BIN:-/usr/local/bin/cli-proxy-api}
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:8317/}
ASSET_SUFFIX=${ASSET_SUFFIX:-_linux_amd64_no-plugin.tar.gz}
SERVICE_USER=${SERVICE_USER:-$(systemctl show -p User --value "$SERVICE")}
: "${SERVICE_USER:?could not determine the service user; set SERVICE_USER}"
GITHUB_API=https://api.github.com
TAG=""
FROM=""

as_user() { runuser -u "$SERVICE_USER" -- "$@"; }

status() { # state message
  local json
  json=$(jq -cn --arg state "$1" --arg message "${2:-}" --arg tag "$TAG" --arg from "$FROM" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{state: $state, tag: $tag, from: $from, message: $message, updated_at: $at}')
  printf '%s\n' "$json" | as_user sh -c 'cat > "$1/.status.tmp" && mv -f "$1/.status.tmp" "$1/status.json"' sh "$UPDATE_DIR" || true
  echo "[$1] ${2:-}"
}

fail() {
  status failed "$1"
  exit 1
}

binary_version() {
  "$1" -h 2>&1 | sed -n 's/.*Version: \([^,]*\),.*/\1/p' | head -n1 || true
}

healthy() { # expected-version
  local pid1 pid2 i
  for i in $(seq 1 45); do
    sleep 1
    if curl -fs -m 2 "$HEALTH_URL" | grep -q "CLI Proxy API Server"; then break; fi
    [ "$i" = 45 ] && { echo "no HTTP response from $HEALTH_URL"; return 1; }
  done
  [ "$(binary_version "$BIN")" = "$1" ] || { echo "installed binary is not $1"; return 1; }
  pid1=$(systemctl show -p MainPID --value "$SERVICE")
  sleep 20
  pid2=$(systemctl show -p MainPID --value "$SERVICE")
  systemctl is-active --quiet "$SERVICE" || { echo "service not active"; return 1; }
  [ "$pid1" = "$pid2" ] && [ "$pid1" != 0 ] || { echo "service restarted ($pid1 -> $pid2)"; return 1; }
  curl -fs -m 2 "$HEALTH_URL" | grep -q "CLI Proxy API Server" || { echo "stopped responding"; return 1; }
}

# Take the request first, so a failure below can't make the path unit retrigger.
if ! REQUEST=$(as_user sh -c 'f="$1/request.json"; [ -f "$f" ] && [ ! -L "$f" ] || exit 3; head -c 4096 "$f"; rm -f "$f"' sh "$UPDATE_DIR"); then
  echo "no update request"
  exit 0
fi
TAG=$(printf '%s' "$REQUEST" | jq -r '.tag // empty' 2>/dev/null || true)
if [[ ! "$TAG" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
  TAG=""
  fail "request has no valid release tag"
fi
VERSION=${TAG#v}
FROM=$(binary_version "$BIN")
[[ "$FROM" =~ ^[0-9A-Za-z.+-]+$ ]] || FROM=""

status downloading "Fetching $TAG from $REPO"
RELEASE=$(curl -fsSL -m 30 -H "Accept: application/vnd.github+json" -H "User-Agent: cliproxy-self-update" \
  "$GITHUB_API/repos/$REPO/releases/tags/$TAG") || fail "release $TAG not found in $REPO"
ASSET="CLIProxyAPI_${VERSION}${ASSET_SUFFIX}"
URL=$(jq -r --arg name "$ASSET" '[.assets[] | select(.name == $name)][0].browser_download_url // empty' <<<"$RELEASE")
DIGEST=$(jq -r --arg name "$ASSET" '[.assets[] | select(.name == $name)][0].digest // empty' <<<"$RELEASE")
EXPECTED_URL="https://github.com/$REPO/releases/download/$TAG/$ASSET"
[ -n "$URL" ] || fail "$TAG has no $ASSET"
[ "${URL,,}" = "${EXPECTED_URL,,}" ] || fail "unexpected download URL for $ASSET"
[[ "$DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "$ASSET has no SHA-256 digest on GitHub"

WORK=$(mktemp -d /var/tmp/cliproxy-update.XXXXXX)
trap 'rm -rf "$WORK"' EXIT
curl -fsSL -m 300 -o "$WORK/release.tar.gz" "$URL" || fail "download of $ASSET failed"

status verifying "Checking $ASSET"
echo "${DIGEST#sha256:}  $WORK/release.tar.gz" | sha256sum -c --quiet - || fail "$ASSET does not match its GitHub digest"
tar -xzf "$WORK/release.tar.gz" -C "$WORK" cli-proxy-api || fail "$ASSET has no cli-proxy-api binary"
chmod 755 "$WORK/cli-proxy-api"
[ "$(binary_version "$WORK/cli-proxy-api")" = "$VERSION" ] || fail "the binary in $ASSET does not report version $VERSION"
if [ "${DRY_RUN:-}" = 1 ]; then
  status verified "Verified $ASSET; not installed (DRY_RUN=1)"
  exit 0
fi

status installing "Installing $VERSION (was ${FROM:-unknown})"
BACKUP="$BIN.${FROM:-previous}.bak"
[ -e "$BACKUP" ] || cp -p "$BIN" "$BACKUP"
install -m 755 -o root -g root "$WORK/cli-proxy-api" "$BIN.new"

status restarting "Restarting $SERVICE on $VERSION"
systemctl stop "$SERVICE"
mv -f "$BIN.new" "$BIN"
systemctl start "$SERVICE" || true
if healthy "$VERSION"; then
  status succeeded "Running $VERSION"
  exit 0
fi

journalctl -u "$SERVICE" --since "-3min" --no-pager | tail -40 || true
systemctl stop "$SERVICE" || true
cp -p "$BACKUP" "$BIN"
systemctl start "$SERVICE" || true
if healthy "${FROM:-}"; then
  status rolled_back "$VERSION did not come up healthy; restored ${FROM:-the previous binary}"
else
  status rollback_failed "$VERSION did not come up healthy and the restored binary is unhealthy too; check the server"
fi
exit 1
