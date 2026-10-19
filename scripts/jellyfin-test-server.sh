#!/usr/bin/env bash
# Start a disposable Jellyfin server in Docker for integration tests.
#
#   scripts/jellyfin-test-server.sh start   # prints the env vars to export
#   scripts/jellyfin-test-server.sh stop
#
# Environment:
#   JELLYFIN_IMAGE  (default jellyfin/jellyfin:12.2)
#   JELLYFIN_PORT   (default 8096)
#   JELLYFIN_NAME   (container name, default mediasync-jellyfin-it)
#
# The server gets a throw-away user (tester / random password) and a music
# library with three generated tones. Nothing here is meant for real use.
set -euo pipefail

IMAGE="${JELLYFIN_IMAGE:-jellyfin/jellyfin:12.2}"
PORT="${JELLYFIN_PORT:-8096}"
NAME="${JELLYFIN_NAME:-mediasync-jellyfin-it}"
URL="http://127.0.0.1:${PORT}"
USER_NAME="tester"

die() { echo "error: $*" >&2; exit 1; }

wait_up() {
  for _ in $(seq 1 90); do
    if curl -fsS "$URL/System/Info/Public" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  die "Jellyfin did not start (docker logs $NAME)"
}

# api: curl that retries while the server is still starting. Jellyfin answers
# /System/Info/Public before startup has finished and returns 503 for other
# endpoints until then (seen on a freshly pulled image in CI).
api() {
  local out
  for _ in $(seq 1 60); do
    if out="$(curl -fsS "$@" 2>/dev/null)"; then
      printf '%s' "$out"
      return 0
    fi
    sleep 2
  done
  curl -fsS "$@"   # final attempt, showing the error
}

start() {
  command -v docker >/dev/null || die "docker is required"
  command -v curl >/dev/null || die "curl is required"
  command -v python3 >/dev/null || die "python3 is required"
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:8096" "$IMAGE" >/dev/null
  wait_up
  PASSWORD="$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 20)"

  api -X POST "$URL/Startup/Configuration" -H 'Content-Type: application/json' \
    -d '{"UICulture":"en-US","MetadataCountryCode":"US","PreferredMetadataLanguage":"en"}' >/dev/null
  api "$URL/Startup/User" >/dev/null
  api -X POST "$URL/Startup/User" -H 'Content-Type: application/json' \
    -d "{\"Name\":\"$USER_NAME\",\"Password\":\"$PASSWORD\"}" >/dev/null
  api -X POST "$URL/Startup/RemoteAccess" -H 'Content-Type: application/json' \
    -d '{"EnableRemoteAccess":true}' >/dev/null
  api -X POST "$URL/Startup/Complete" >/dev/null

  # Generate a few tagged audio files with the ffmpeg bundled in the image.
  docker exec "$NAME" sh -c 'mkdir -p /media/music/ArtistA/AlbumX && cd /media/music/ArtistA/AlbumX &&
    for i in 1 2 3; do /usr/lib/jellyfin-ffmpeg/ffmpeg -loglevel error -y -f lavfi \
      -i "sine=frequency=$((300+i*100)):duration=180" -metadata title="Track $i" -metadata artist="Artist A" \
      -metadata album="Album X" -metadata album_artist="Artist A" -metadata track=$i 0$i.mp3; done'

  AUTH='MediaBrowser Client="mediasync-setup", Device="setup", DeviceId="mediasync-setup", Version="1"'
  TOKEN="$(api -X POST "$URL/Users/AuthenticateByName" -H "Authorization: $AUTH" \
    -H 'Content-Type: application/json' -d "{\"Username\":\"$USER_NAME\",\"Pw\":\"$PASSWORD\"}" |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["AccessToken"])')"
  H="Authorization: MediaBrowser Token=\"$TOKEN\""
  api -X POST "$URL/Library/VirtualFolders?name=Music&collectionType=music&refreshLibrary=true" \
    -H "$H" -H 'Content-Type: application/json' -d '{"LibraryOptions":{"PathInfos":[{"Path":"/media/music"}]}}' >/dev/null
  for _ in $(seq 1 30); do
    n="$(api "$URL/Items?recursive=true&includeItemTypes=Audio" -H "$H" |
      python3 -c 'import json,sys; print(json.load(sys.stdin)["TotalRecordCount"])')"
    [ "$n" -ge 3 ] && break
    sleep 2
  done
  curl -fsS -X POST "$URL/Sessions/Logout" -H "$H" >/dev/null || true
  version="$(curl -fsS "$URL/System/Info/Public" | python3 -c 'import json,sys; print(json.load(sys.stdin)["Version"])')"
  echo "# Jellyfin $version ready ($n audio items). Export:" >&2
  echo "export MEDIASYNC_IT_JELLYFIN_URL=$URL"
  echo "export MEDIASYNC_IT_JELLYFIN_USER=$USER_NAME"
  echo "export MEDIASYNC_IT_JELLYFIN_PASSWORD=$PASSWORD"
}

case "${1:-}" in
  start) start ;;
  stop) docker rm -f "$NAME" >/dev/null && echo "stopped $NAME" ;;
  *) echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
