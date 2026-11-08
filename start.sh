#!/usr/bin/env bash
# GoHomeAssistant startup script. Reads its configuration from .env.
#
#   ./start.sh          build the image and (re)start the container
#   ./start.sh logs     follow the logs
#   ./start.sh stop     stop and remove the container (projects and data are kept)
#   ./start.sh dev      run without Docker (needs Go and Node; no debugger on macOS)
set -euo pipefail
cd "$(dirname "$0")"

NAME=gohomeassistant
IMAGE=gohomeassistant:local

if [[ ! -f .env ]]; then
  echo "No .env found. Create it from the template and fill in HA_URL and HA_TOKEN:" >&2
  echo "  cp .env.example .env" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1091
source ./.env
set +a
: "${HA_URL:?HA_URL is missing in .env}"
: "${HA_TOKEN:?HA_TOKEN is missing in .env}"
PORT=${GHA_PORT:-8099}
BIND=${GHA_BIND:-127.0.0.1}
URL="http://$([[ $BIND == 0.0.0.0 ]] && echo localhost || echo "$BIND"):$PORT"

case "${1:-up}" in
up)
  docker build -t "$IMAGE" .
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  mounts=(-v gha-data:/data)
  if [[ -n ${GHA_PROJECTS_HOST_DIR:-} ]]; then
    mkdir -p "$GHA_PROJECTS_HOST_DIR"
    mounts+=(-v "$(cd "$GHA_PROJECTS_HOST_DIR" && pwd):/data/projects")
  fi
  docker run -d --name "$NAME" --restart unless-stopped \
    -p "$BIND:$PORT:8099" --env-file .env "${mounts[@]}" "$IMAGE" >/dev/null
  echo "GoHomeAssistant is running: $URL"
  echo "Logs: ./start.sh logs    Stop: ./start.sh stop"
  ;;
logs)
  docker logs -f "$NAME"
  ;;
stop)
  docker rm -f "$NAME" >/dev/null && echo "stopped"
  ;;
dev)
  (cd web && { [[ -d node_modules ]] || npm ci --no-audit --no-fund; } && npm run build)
  projects=${GHA_PROJECTS_HOST_DIR:-./projects}
  mkdir -p "$projects" data
  echo "GoHomeAssistant (dev): $URL"
  GHA_PROJECTS_DIR=$projects GHA_DATA_DIR=./data GHA_SDK_DIR=./sdk GHA_HTTP_ADDR="$BIND:$PORT" \
    exec go run ./cmd/gohomeassistant
  ;;
*)
  sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
  ;;
esac
