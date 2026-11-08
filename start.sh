#!/usr/bin/env bash
# GoHomeAssistant startup script. Reads its configuration from .env.
#
#   ./start.sh          build the image and (re)start the container
#   ./start.sh logs     follow the logs
#   ./start.sh stop     stop and remove the container (projects and data are kept)
#   ./start.sh dev      run without Docker (needs Go and Node; no debugger on macOS)
#   ./start.sh addon    build an installable Home Assistant add-on in dist/addon/
set -euo pipefail
cd "$(dirname "$0")"

NAME=gohomeassistant
IMAGE=gohomeassistant:local

load_env() {
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
}

case "${1:-up}" in
up)
  load_env
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
  load_env
  (cd web && { [[ -d node_modules ]] || npm ci --no-audit --no-fund; } && npm run build)
  projects=${GHA_PROJECTS_HOST_DIR:-./projects}
  mkdir -p "$projects" data
  echo "GoHomeAssistant (dev): $URL"
  GHA_PROJECTS_DIR=$projects GHA_DATA_DIR=./data GHA_SDK_DIR=./sdk GHA_HTTP_ADDR="$BIND:$PORT" \
    exec go run ./cmd/gohomeassistant
  ;;
addon)
  # The Supervisor builds a local add-on from its own folder, so the folder
  # must contain the sources. Only files tracked by git are copied (never
  # .env or local data).
  out=dist/addon/gohomeassistant
  rm -rf dist/addon
  mkdir -p "$out"
  cp addon/config.yaml addon/DOCS.md addon/README.md addon/CHANGELOG.md "$out/"
  git ls-files -z -- Dockerfile .dockerignore go.mod go.sum cmd internal sdk web \
    | tar --null -T - -cf - | tar -xf - -C "$out"
  untracked=$(git ls-files --others --exclude-standard -- cmd internal sdk web)
  if [[ -n $untracked ]]; then
    echo "warning: untracked files are not included:" >&2
    echo "$untracked" >&2
  fi
  tar -czf dist/gohomeassistant-addon.tar.gz -C dist/addon gohomeassistant
  version=$(sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\}/\1/p' addon/config.yaml)
  cat <<EOT
Add-on $version built: $out  (also dist/gohomeassistant-addon.tar.gz)

Install it as a local add-on:
  1. Copy the folder "gohomeassistant" into the "addons" folder of your HA:
     - Samba add-on: open the "addons" share and copy it there, or
     - SSH add-on:   scp -r $out root@<your-ha>:/addons/
  2. HA: Settings > Add-ons > Add-on Store > menu (top right) > Check for updates
  3. Open "GoHomeAssistant" under "Local add-ons", Install (builds on your HA,
     takes a few minutes), then Start. It appears in the sidebar.
EOT
  ;;
*)
  sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
  ;;
esac
