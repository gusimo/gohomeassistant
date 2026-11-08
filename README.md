# GoHomeAssistant

Write Home Assistant automations in Go. Each project is a directory with a
`go.mod`; GoHomeAssistant builds and runs it, and rebuilds it on every change.
Neither Home Assistant nor GoHomeAssistant restarts: only the project process
is swapped, and only if the new version compiles.

## How it works

```
GoHomeAssistant host ── one WebSocket ──► Home Assistant
   │  watches projects/, builds with `go build`, supervises processes
   │  unix socket (HA protocol subset, ids remapped per project)
   ├── project A  (import ha "github.com/gusimo/gohomeassistant/sdk")
   └── project B
```

- `internal/ha`: reconnecting HA client; subscriptions survive reconnects.
- `internal/bridge`: shares the HA connection with projects and removes a project's subscriptions when it exits.
- `internal/project`: discovery, file watching, build, process swap, restart with backoff.
- `sdk/`: the module projects import. It has no dependencies, keeps a state cache and runs handlers serially.

Projects don't need a `replace` directive for the SDK: the host builds each
one with a generated `go.work`.

## Typed entities

GoHomeAssistant generates the module `github.com/gusimo/gohomeassistant/ent`
from your instance (under `$GHA_DATA_DIR/gen/ent`, not inside your projects):

```go
import (
	"github.com/gusimo/gohomeassistant/ent"
	ha "github.com/gusimo/gohomeassistant/sdk"
)

ha.MustConnect()
ent.BinarySensor.FrontDoor.OnChange(func(ch ha.StateChange) {
	if ent.BinarySensor.FrontDoor.IsOn() {
		ent.Light.Hall.TurnOn(ent.LightTurnOn{BrightnessPct: 80, Transition: 2})
	}
})
w, ok := ent.Sensor.Power.Float()                     // numeric state
ent.Svc.Light.TurnOff(ha.Target{AreaID: []string{"kitchen"}})
ent.Svc.Notify.MobileAppPhone(ent.NotifyMobileAppPhone{Message: "hi"})
ha.Default().Run()
```

- Every entity is a field of its domain (`ent.Light`, `ent.Sensor`, …). Every
  service that targets that domain is a method on it. Parameters are optional
  structs, typed from HA's selectors, and zero values are not sent. Use `Extra`
  for anything else.
- `ent.Svc.<Domain>.<Service>` lists all services, including those without a
  target (scripts, notify).
- Regeneration happens when entities or services are added or removed. All
  projects are rebuilt, but a process is only restarted if its binary actually
  changed. If an entity a project uses disappears, the build fails and the
  previous version keeps running.

## Web UI

Open `http://<host>:8099` (`GHA_HTTP_ADDR`). You get projects with live
status, files, a Monaco editor (⌘S / Ctrl+S saves and hot-reloads, build
errors show up as markers) and a live log panel. **There is no authentication
yet**, so only expose it on a trusted network.

Frontend development: `cd web && npm install && npm run dev` starts Vite on
:5173 and proxies `/api` to a running GoHomeAssistant on :8099.
`npm run build` writes into `internal/server/dist/app`, which is embedded into
the Go binary.

## Run locally

```sh
HA_URL=http://homeassistant.local:8123 HA_TOKEN=<long-lived token> \
GHA_PROJECTS_DIR=./examples/projects GHA_DATA_DIR=./data GHA_SDK_DIR=./sdk \
go run ./cmd/gohomeassistant
```

Build the UI first (`cd web && npm install && npm run build`); otherwise
`/` only shows a hint.

Edit `examples/projects/hello/main.go` and save to see it reload.

## Docker (standalone)

```sh
docker build -t gohomeassistant .
docker run -p 8099:8099 -v gha-data:/data -e HA_URL=... -e HA_TOKEN=... gohomeassistant
```

Inside the add-on, `SUPERVISOR_TOKEN` is picked up automatically.

**Editing on macOS/Windows with a bind mount** (`-v ./projects:/data/projects`):
file change events don't cross the Docker VM boundary, so add
`-e GHA_WATCH_POLL=1s`. With Rancher Desktop's default reverse-sshfs mounts,
changes take about 5–10s to become visible in the container because sshfs
caches. Switching Rancher Desktop to VZ with virtiofs mounts makes them
visible almost immediately.

## Tests

```sh
go test -race ./...
```

The end-to-end test runs a fake HA server and real project builds. It covers
hot reload, a broken build keeping the old version running, and cleanup on
project removal.

## Roadmap

1. ✅ Host, SDK, hot reload
2. ✅ Typed entity and service code generation (`ent.Light.Kitchen.TurnOn()`)
3. Web UI
   - ✅ 3a: projects, files, editor, build markers, live logs
   - 3b: gopls code completion
   - 3c: HA login (Ingress or OAuth), entity browser
4. In-UI debugging (Delve DAP)
5. Git: local first, then remotes
6. HA add-on packaging (config.yaml, Ingress)
